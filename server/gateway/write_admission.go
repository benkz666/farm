package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/shared/store"
	"farm/server/shared/telemetry"

	"github.com/redis/go-redis/v9"
)

// WriteBacklog is a point-in-time aggregate of every Farm write-journal
// consumer group. Pending records have been delivered but not ACKed; lag
// records have not yet been delivered to a Projector.
type WriteBacklog struct {
	Pending int64
	Lag     int64
	Streams int
}

func (backlog WriteBacklog) Total() int64 {
	return saturatingBacklogAdd(backlog.Pending, backlog.Lag)
}

// WriteBacklogSource provides the durable downstream pressure observed by a
// Gateway. Implementations must return a deployment-wide aggregate rather than
// a process-local queue length.
type WriteBacklogSource interface {
	Snapshot(context.Context) (WriteBacklog, error)
}

// DynamicWriteAdmissionConfig controls the closed-loop Gateway write guard.
// Limits are per Gateway; watermarks are deployment-wide Redis record counts.
type DynamicWriteAdmissionConfig struct {
	MinLimit      int
	MaxLimit      int
	LowWatermark  int64
	HighWatermark int64
	HardWatermark int64
	RecoveryStep  int
	PollInterval  time.Duration
	SampleTimeout time.Duration
	ErrorGrace    time.Duration
	AdmissionWait time.Duration
}

// DefaultDynamicWriteAdmissionConfig keeps the configured ceiling (512 in the
// current manifests) while allowing real Redis pressure to reduce it. The
// watermarks are multiples of the deployed 4 Projectors x 1024 record batch.
func DefaultDynamicWriteAdmissionConfig(maxLimit int) DynamicWriteAdmissionConfig {
	minLimit := max(1, maxLimit/8)
	return DynamicWriteAdmissionConfig{
		MinLimit:      minLimit,
		MaxLimit:      maxLimit,
		LowWatermark:  8_192,
		HighWatermark: 65_536,
		HardWatermark: 262_144,
		RecoveryStep:  max(1, maxLimit/8),
		PollInterval:  250 * time.Millisecond,
		SampleTimeout: 150 * time.Millisecond,
		ErrorGrace:    2 * time.Second,
		AdmissionWait: admissionWait,
	}
}

func (config DynamicWriteAdmissionConfig) validate() error {
	switch {
	case config.MinLimit <= 0:
		return errors.New("minimum limit must be positive")
	case config.MaxLimit < config.MinLimit:
		return errors.New("maximum limit must be greater than or equal to minimum limit")
	case config.LowWatermark < 0:
		return errors.New("low watermark must be non-negative")
	case config.HighWatermark <= config.LowWatermark:
		return errors.New("high watermark must be greater than low watermark")
	case config.HardWatermark <= config.HighWatermark:
		return errors.New("hard watermark must be greater than high watermark")
	case config.RecoveryStep <= 0:
		return errors.New("recovery step must be positive")
	case config.PollInterval <= 0:
		return errors.New("poll interval must be positive")
	case config.SampleTimeout <= 0:
		return errors.New("sample timeout must be positive")
	case config.ErrorGrace < 0:
		return errors.New("error grace must be non-negative")
	case config.AdmissionWait < 0:
		return errors.New("admission wait must be non-negative")
	default:
		return nil
	}
}

// DynamicWriteAdmission bounds foreground writes using actual Redis Stream
// pressure. Reductions never cancel an admitted request; they only prevent new
// requests from entering until the in-flight count falls below the new limit.
type DynamicWriteAdmission struct {
	source WriteBacklogSource
	config DynamicWriteAdmissionConfig

	limit     atomic.Int64
	inFlight  atomic.Int64
	pending   atomic.Int64
	lag       atomic.Int64
	started   atomic.Bool
	lastOK    atomic.Int64
	released  chan struct{}
	metricsMu sync.RWMutex
	metrics   *telemetry.Metrics
}

func NewDynamicWriteAdmission(
	source WriteBacklogSource,
	config DynamicWriteAdmissionConfig,
) (*DynamicWriteAdmission, error) {
	if source == nil {
		return nil, errors.New("write backlog source is nil")
	}
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid dynamic write admission config: %w", err)
	}
	admission := &DynamicWriteAdmission{
		source:   source,
		config:   config,
		released: make(chan struct{}, 1),
	}
	admission.limit.Store(int64(config.MaxLimit))
	admission.lastOK.Store(time.Now().UnixNano())
	return admission, nil
}

func (admission *DynamicWriteAdmission) SetMetrics(metrics *telemetry.Metrics) {
	if admission == nil {
		return
	}
	admission.metricsMu.Lock()
	admission.metrics = metrics
	admission.metricsMu.Unlock()
	admission.publishMetrics(false)
}

func (admission *DynamicWriteAdmission) Start(ctx context.Context) {
	if admission == nil || !admission.started.CompareAndSwap(false, true) {
		return
	}
	go admission.run(ctx)
}

func (admission *DynamicWriteAdmission) run(ctx context.Context) {
	admission.sample(ctx)
	ticker := time.NewTicker(admission.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			admission.sample(ctx)
		}
	}
}

func (admission *DynamicWriteAdmission) sample(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, admission.config.SampleTimeout)
	backlog, err := admission.source.Snapshot(ctx)
	cancel()
	if err != nil {
		admission.handleSampleError(time.Now())
		return
	}
	admission.lastOK.Store(time.Now().UnixNano())
	admission.pending.Store(max(0, backlog.Pending))
	admission.lag.Store(max(0, backlog.Lag))
	admission.applyBacklog(backlog.Total())
	admission.publishMetrics(false)
}

func (admission *DynamicWriteAdmission) handleSampleError(now time.Time) {
	lastOK := admission.lastOK.Load()
	if lastOK == 0 || now.Sub(time.Unix(0, lastOK)) >= admission.config.ErrorGrace {
		// Unknown durable pressure must not fail open forever. Decay gradually so
		// a short Redis control-plane hiccup does not cause a traffic cliff.
		current := admission.Limit()
		admission.limit.Store(int64(max(admission.config.MinLimit, current-admission.config.RecoveryStep)))
	}
	admission.publishMetrics(true)
}

func (admission *DynamicWriteAdmission) applyBacklog(backlog int64) {
	backlog = max(0, backlog)
	current := admission.Limit()
	target := admission.targetLimit(backlog)
	if target == current {
		return
	}
	if target > current {
		// Recovery is deliberately additive. Three Gateways therefore do not all
		// jump from the minimum to maximum on the first empty sample.
		target = min(target, current+admission.config.RecoveryStep)
	} else if backlog < admission.config.HardWatermark {
		// Reduce by at most 25% per sample below the hard watermark. At the hard
		// watermark the minimum is applied immediately.
		decrease := max(admission.config.RecoveryStep, current/4)
		target = max(target, current-decrease)
	}
	admission.limit.Store(int64(target))
}

func (admission *DynamicWriteAdmission) targetLimit(backlog int64) int {
	config := admission.config
	if backlog <= config.LowWatermark {
		return config.MaxLimit
	}
	if backlog >= config.HardWatermark {
		return config.MinLimit
	}
	middle := max(config.MinLimit, (config.MaxLimit+config.MinLimit)/2)
	if backlog <= config.HighWatermark {
		return interpolateLimit(
			config.MaxLimit, middle,
			backlog-config.LowWatermark,
			config.HighWatermark-config.LowWatermark,
		)
	}
	return interpolateLimit(
		middle, config.MinLimit,
		backlog-config.HighWatermark,
		config.HardWatermark-config.HighWatermark,
	)
}

func interpolateLimit(start, end int, progress, span int64) int {
	if span <= 0 || progress <= 0 {
		return start
	}
	if progress >= span {
		return end
	}
	delta := int64(end - start)
	return start + int(delta*progress/span)
}

func (admission *DynamicWriteAdmission) Acquire() bool {
	if admission == nil {
		return true
	}
	if admission.tryAcquire() {
		return true
	}
	if admission.config.AdmissionWait <= 0 {
		return false
	}
	timer := time.NewTimer(admission.config.AdmissionWait)
	defer timer.Stop()
	for {
		select {
		case <-admission.released:
			if admission.tryAcquire() {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

func (admission *DynamicWriteAdmission) tryAcquire() bool {
	for {
		inFlight := admission.inFlight.Load()
		if inFlight >= admission.limit.Load() {
			return false
		}
		if admission.inFlight.CompareAndSwap(inFlight, inFlight+1) {
			return true
		}
	}
}

func (admission *DynamicWriteAdmission) Release() {
	if admission == nil {
		return
	}
	if remaining := admission.inFlight.Add(-1); remaining < 0 {
		admission.inFlight.Store(0)
	}
	select {
	case admission.released <- struct{}{}:
	default:
	}
}

func (admission *DynamicWriteAdmission) Limit() int {
	if admission == nil {
		return 0
	}
	return int(admission.limit.Load())
}

func (admission *DynamicWriteAdmission) publishMetrics(sampleError bool) {
	admission.metricsMu.RLock()
	metrics := admission.metrics
	admission.metricsMu.RUnlock()
	if metrics != nil {
		metrics.SetGatewayWriteAdmission(
			admission.Limit(),
			admission.pending.Load(),
			admission.lag.Load(),
			sampleError,
		)
	}
}

func saturatingBacklogAdd(left, right int64) int64 {
	left, right = max(0, left), max(0, right)
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

// RedisWriteBacklogSource pipelines XINFO GROUPS across every configured Farm
// stream. Missing streams or consumer groups make the complete observation
// invalid, preventing a partially started deployment from underreporting lag.
type RedisWriteBacklogSource struct {
	client redis.UniversalClient
	keys   []string
	group  string
}

func NewRedisWriteBacklogSource(
	client redis.UniversalClient,
	prefix string,
	farmIDs []string,
	shards int,
) (*RedisWriteBacklogSource, error) {
	if client == nil {
		return nil, errors.New("event Redis client is nil")
	}
	if strings.Trim(strings.TrimSpace(prefix), ":") == "" {
		return nil, errors.New("write journal prefix is empty")
	}
	if shards <= 0 {
		return nil, errors.New("write journal shards must be positive")
	}
	uniqueIDs := make(map[string]struct{}, len(farmIDs))
	for _, farmID := range farmIDs {
		farmID = strings.TrimSpace(farmID)
		if farmID != "" {
			uniqueIDs[farmID] = struct{}{}
		}
	}
	if len(uniqueIDs) == 0 {
		return nil, errors.New("at least one Farm instance is required")
	}
	orderedIDs := make([]string, 0, len(uniqueIDs))
	for farmID := range uniqueIDs {
		orderedIDs = append(orderedIDs, farmID)
	}
	sort.Strings(orderedIDs)
	keys := make([]string, 0, len(orderedIDs)*shards)
	for _, farmID := range orderedIDs {
		for shard := range shards {
			keys = append(keys, store.FarmWriteJournalStreamKey(prefix, farmID, shard))
		}
	}
	return &RedisWriteBacklogSource{
		client: client,
		keys:   keys,
		group:  store.FarmWriteJournalProjectorGroup,
	}, nil
}

func (source *RedisWriteBacklogSource) Snapshot(ctx context.Context) (WriteBacklog, error) {
	if source == nil || source.client == nil || len(source.keys) == 0 {
		return WriteBacklog{}, errors.New("write backlog source is not configured")
	}
	pipe := source.client.Pipeline()
	commands := make([]*redis.XInfoGroupsCmd, len(source.keys))
	for index, key := range source.keys {
		commands[index] = pipe.XInfoGroups(ctx, key)
	}
	_, _ = pipe.Exec(ctx)

	var snapshot WriteBacklog
	for index, command := range commands {
		groups, err := command.Result()
		if err != nil {
			if missingRedisStream(err) {
				continue
			}
			return WriteBacklog{}, fmt.Errorf("read write backlog for %s: %w", source.keys[index], err)
		}
		found := false
		for _, group := range groups {
			if group.Name != source.group {
				continue
			}
			lag := group.Lag
			if lag < 0 {
				// Redis can temporarily report unknown lag after stream entry
				// deletion. Processed records are XDEL'ed, so XLEN is the exact
				// remaining pending+undelivered count for this recovery stream.
				length, lengthErr := source.client.XLen(ctx, source.keys[index]).Result()
				if lengthErr != nil {
					return WriteBacklog{}, fmt.Errorf("read unknown write backlog for %s: %w", source.keys[index], lengthErr)
				}
				lag = max(0, length-group.Pending)
			}
			snapshot.Pending = saturatingBacklogAdd(snapshot.Pending, group.Pending)
			snapshot.Lag = saturatingBacklogAdd(snapshot.Lag, lag)
			snapshot.Streams++
			found = true
			break
		}
		if !found {
			return WriteBacklog{}, fmt.Errorf("write journal group %q missing for %s", source.group, source.keys[index])
		}
	}
	if snapshot.Streams != len(source.keys) {
		return WriteBacklog{}, fmt.Errorf(
			"incomplete write backlog snapshot: got %d of %d streams",
			snapshot.Streams, len(source.keys),
		)
	}
	return snapshot, nil
}

func missingRedisStream(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such key") || strings.Contains(message, "no such stream")
}
