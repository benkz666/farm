package farmrpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

const defaultAdmissionWait = 5 * time.Millisecond

// WriteBacklogSource exposes the local durable-journal pressure owned by Farm.
type WriteBacklogSource interface {
	WriteBacklog(context.Context) (store.WriteJournalBacklog, error)
}

// WriteAdmissionConfig controls Farm's closed-loop foreground write guard.
// The guard protects the durable Redis journal and its MySQL projector without
// leaking persistence knowledge into Gateway.
type WriteAdmissionConfig struct {
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

func DefaultWriteAdmissionConfig(maxLimit int) WriteAdmissionConfig {
	return WriteAdmissionConfig{
		MinLimit:      max(1, maxLimit/8),
		MaxLimit:      maxLimit,
		LowWatermark:  8_192,
		HighWatermark: 65_536,
		HardWatermark: 262_144,
		RecoveryStep:  max(1, maxLimit/8),
		PollInterval:  250 * time.Millisecond,
		SampleTimeout: 150 * time.Millisecond,
		ErrorGrace:    2 * time.Second,
		AdmissionWait: defaultAdmissionWait,
	}
}

func (config WriteAdmissionConfig) validate() error {
	switch {
	case config.MinLimit <= 0:
		return errors.New("minimum limit must be positive")
	case config.MaxLimit < config.MinLimit:
		return errors.New("maximum limit must be at least the minimum")
	case config.LowWatermark < 0:
		return errors.New("low watermark must be non-negative")
	case config.HighWatermark <= config.LowWatermark:
		return errors.New("high watermark must exceed the low watermark")
	case config.HardWatermark <= config.HighWatermark:
		return errors.New("hard watermark must exceed the high watermark")
	case config.RecoveryStep <= 0:
		return errors.New("recovery step must be positive")
	case config.PollInterval <= 0 || config.SampleTimeout <= 0:
		return errors.New("sampling durations must be positive")
	case config.ErrorGrace < 0 || config.AdmissionWait < 0:
		return errors.New("grace and wait durations must be non-negative")
	default:
		return nil
	}
}

// DynamicWriteAdmission admits Farm write commands according to actual local
// Redis Stream pending+lag. Already admitted commands are never cancelled.
type DynamicWriteAdmission struct {
	source WriteBacklogSource
	config WriteAdmissionConfig

	limit     atomic.Int64
	inFlight  atomic.Int64
	pending   atomic.Int64
	lag       atomic.Int64
	started   atomic.Bool
	lastOK    atomic.Int64
	releaseMu sync.Mutex
	released  chan struct{}

	metricsMu sync.RWMutex
	metrics   *telemetry.Metrics
}

func NewDynamicWriteAdmission(source WriteBacklogSource, config WriteAdmissionConfig) (*DynamicWriteAdmission, error) {
	if source == nil {
		return nil, errors.New("write backlog source is nil")
	}
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid Farm write admission config: %w", err)
	}
	admission := &DynamicWriteAdmission{source: source, config: config, released: make(chan struct{})}
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
	backlog, err := admission.source.WriteBacklog(ctx)
	cancel()
	if err != nil {
		admission.handleSampleError(time.Now())
		return
	}
	admission.lastOK.Store(time.Now().UnixNano())
	admission.pending.Store(max(int64(0), backlog.Pending))
	admission.lag.Store(max(int64(0), backlog.Lag))
	admission.applyBacklog(backlog.Total())
	admission.publishMetrics(false)
}

func (admission *DynamicWriteAdmission) handleSampleError(now time.Time) {
	lastOK := admission.lastOK.Load()
	if lastOK == 0 || now.Sub(time.Unix(0, lastOK)) >= admission.config.ErrorGrace {
		current := admission.Limit()
		admission.limit.Store(int64(max(admission.config.MinLimit, current-admission.config.RecoveryStep)))
	}
	admission.publishMetrics(true)
}

func (admission *DynamicWriteAdmission) applyBacklog(backlog int64) {
	current := admission.Limit()
	target := admission.targetLimit(max(int64(0), backlog))
	if target == current {
		return
	}
	if target > current {
		target = min(target, current+admission.config.RecoveryStep)
	} else if backlog < admission.config.HardWatermark {
		target = max(target, current-max(admission.config.RecoveryStep, current/4))
	}
	admission.limit.Store(int64(target))
	if target > current {
		admission.wakeWaiters()
	}
}

func (admission *DynamicWriteAdmission) targetLimit(backlog int64) int {
	config := admission.config
	switch {
	case backlog <= config.LowWatermark:
		return config.MaxLimit
	case backlog >= config.HardWatermark:
		return config.MinLimit
	case backlog <= config.HighWatermark:
		middle := max(config.MinLimit, (config.MaxLimit+config.MinLimit)/2)
		return interpolateAdmissionLimit(config.MaxLimit, middle, backlog-config.LowWatermark, config.HighWatermark-config.LowWatermark)
	default:
		middle := max(config.MinLimit, (config.MaxLimit+config.MinLimit)/2)
		return interpolateAdmissionLimit(middle, config.MinLimit, backlog-config.HighWatermark, config.HardWatermark-config.HighWatermark)
	}
}

func interpolateAdmissionLimit(start, end int, progress, span int64) int {
	if progress <= 0 || span <= 0 {
		return start
	}
	if progress >= span {
		return end
	}
	return start + int(int64(end-start)*progress/span)
}

func (admission *DynamicWriteAdmission) Acquire() bool {
	if admission == nil || admission.tryAcquire() {
		return true
	}
	if admission.config.AdmissionWait <= 0 {
		admission.observeRejection()
		return false
	}
	timer := time.NewTimer(admission.config.AdmissionWait)
	defer timer.Stop()
	for {
		released := admission.releaseSignal()
		// A release can happen between the first tryAcquire and taking the
		// generation snapshot. Recheck after the snapshot so that available
		// capacity is never hidden behind a future wake-up.
		if admission.tryAcquire() {
			return true
		}
		select {
		case <-released:
			if admission.tryAcquire() {
				return true
			}
		case <-timer.C:
			admission.observeRejection()
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
	admission.wakeWaiters()
}

func (admission *DynamicWriteAdmission) releaseSignal() <-chan struct{} {
	admission.releaseMu.Lock()
	released := admission.released
	admission.releaseMu.Unlock()
	return released
}

func (admission *DynamicWriteAdmission) wakeWaiters() {
	admission.releaseMu.Lock()
	close(admission.released)
	admission.released = make(chan struct{})
	admission.releaseMu.Unlock()
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
		metrics.SetFarmWriteAdmission(admission.Limit(), admission.pending.Load(), admission.lag.Load(), sampleError)
	}
}

func (admission *DynamicWriteAdmission) observeRejection() {
	admission.metricsMu.RLock()
	metrics := admission.metrics
	admission.metricsMu.RUnlock()
	if metrics != nil {
		metrics.ObserveFarmWriteRejected()
	}
}
