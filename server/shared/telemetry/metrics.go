package telemetry

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds process metrics registered on an injectable registry.
// Never uses prometheus.DefaultRegisterer (avoids duplicate-registration panics in tests).
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec

	WSConnections         prometheus.Gauge
	WSRequests            *prometheus.CounterVec
	WSDuration            *prometheus.HistogramVec
	WSDisconnects         *prometheus.CounterVec
	WSHandshakeErrors     *prometheus.CounterVec
	WSSessionReplacements prometheus.Counter
	WSWriteQueueDepth     prometheus.Histogram
	WSWriteQueueFull      prometheus.Counter
	WSWriteFailures       *prometheus.CounterVec
	// Hot successful commands use pre-bound metric children. This avoids two
	// label string formats plus two MetricVec hash/lock lookups per request.
	// The maps are immutable after construction and therefore safe for
	// concurrent lock-free reads.
	wsOKRequests map[uint32]prometheus.Counter
	wsDurations  map[uint32]prometheus.Observer

	ActorResident     prometheus.Gauge
	ActorMailboxDepth prometheus.Histogram
	ActorDoBusy       prometheus.Counter
	ActorLoadDuration prometheus.Histogram
	ActorLoadErrors   prometheus.Counter
	ActorSaveDuration prometheus.Histogram
	ActorSaveErrors   prometheus.Counter
	CommitBatches     prometheus.Counter
	CommitFarms       prometheus.Counter
	CommitRequests    prometheus.Counter

	WriteJournalAppends            prometheus.Counter
	WriteJournalAppendRecords      prometheus.Counter
	WriteJournalAppendDuration     prometheus.Histogram
	WriteJournalAppendErrors       prometheus.Counter
	WriteJournalProjectionBatches  prometheus.Counter
	WriteJournalProjectionRecords  prometheus.Counter
	WriteJournalProjectionDuration prometheus.Histogram
	WriteJournalProjectionErrors   prometheus.Counter

	DeltaBatches        prometheus.Counter
	DeltaTargets        prometheus.Histogram
	DeltaEncodeDuration prometheus.Histogram
	DeltaPushDuration   prometheus.Histogram
}

// NewMetrics registers collectors on reg (or a fresh registry when reg is nil).
func NewMetrics(reg *prometheus.Registry) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &Metrics{Registry: reg}

	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_http_requests_total",
		Help: "HTTP requests handled by the public/business listener",
	}, []string{"method", "route", "code"})
	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "farm_http_request_duration_seconds",
		Help:    "HTTP request latency on the business listener",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	m.WSConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "farm_ws_connections",
		Help: "Current WebSocket connections on this process",
	})
	m.WSRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_ws_requests_total",
		Help: "WebSocket command requests",
	}, []string{"cmd", "code"})
	m.WSDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "farm_ws_request_duration_seconds",
		Help:    "WebSocket command handling latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"cmd"})
	m.WSDisconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_ws_disconnects_total",
		Help: "Closed WebSocket connections classified by bounded server-side reason",
	}, []string{"reason"})
	m.WSHandshakeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_ws_handshake_errors_total",
		Help: "Rejected WebSocket handshakes classified by protocol error code",
	}, []string{"code"})
	m.WSSessionReplacements = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_ws_session_replacements_total",
		Help: "Established WebSocket sessions closed because a newer session replaced them",
	})
	m.WSWriteQueueDepth = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_ws_write_queue_depth",
		Help:    "Pending response plus push records observed after enqueue",
		Buckets: []float64{0, 1, 2, 4, 8, 16, 32, 64, 68},
	})
	m.WSWriteQueueFull = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_ws_write_queue_full_total",
		Help: "Push records rejected because a connection write queue was full",
	})
	m.WSWriteFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "farm_ws_write_failures_total",
		Help: "WebSocket physical write failures classified by response or push path",
	}, []string{"path"})
	m.wsOKRequests = make(map[uint32]prometheus.Counter, len(knownWSCommands))
	m.wsDurations = make(map[uint32]prometheus.Observer, len(knownWSCommands))
	for _, cmd := range knownWSCommands {
		label := strconv.FormatUint(uint64(cmd), 10)
		m.wsOKRequests[cmd] = m.WSRequests.WithLabelValues(label, "0")
		m.wsDurations[cmd] = m.WSDuration.WithLabelValues(label)
	}

	m.ActorResident = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "farm_actor_resident",
		Help: "Number of resident farm Actors in this process",
	})
	m.ActorMailboxDepth = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_actor_mailbox_depth",
		Help:    "Observed Actor mailbox queue depth at sample points",
		Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100},
	})
	m.ActorDoBusy = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_actor_do_busy_total",
		Help: "Do calls rejected because the Actor mailbox was busy",
	})
	m.ActorLoadDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_actor_load_duration_seconds",
		Help:    "Actor farm load latency",
		Buckets: prometheus.DefBuckets,
	})
	m.ActorLoadErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_actor_load_errors_total",
		Help: "Actor farm load failures",
	})
	m.ActorSaveDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_actor_save_duration_seconds",
		Help:    "Actor farm save/flush latency",
		Buckets: prometheus.DefBuckets,
	})
	m.ActorSaveErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_actor_save_errors_total",
		Help: "Actor farm save/flush failures",
	})
	m.CommitBatches = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_committer_batches_total",
		Help: "Group-commit batches attempted",
	})
	m.CommitFarms = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_committer_farms_total",
		Help: "Distinct farms included across group-commit batches",
	})
	m.CommitRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_committer_requests_total",
		Help: "Logical snapshot requests merged into group-commit batches",
	})
	m.WriteJournalAppends = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_write_journal_appends_total",
		Help: "Redis Streams append batches attempted by the Farm write journal",
	})
	m.WriteJournalAppendRecords = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_write_journal_append_records_total",
		Help: "Farm, task, codex and outbox records submitted to the write journal",
	})
	m.WriteJournalAppendDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_write_journal_append_duration_seconds",
		Help:    "Redis Streams durable append latency observed by request paths",
		Buckets: prometheus.DefBuckets,
	})
	m.WriteJournalAppendErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_write_journal_append_errors_total",
		Help: "Redis Streams append failures",
	})
	m.WriteJournalProjectionBatches = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_write_journal_projection_batches_total",
		Help: "MySQL materialization batches completed or attempted",
	})
	m.WriteJournalProjectionRecords = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_write_journal_projection_records_total",
		Help: "Journal records included in MySQL materialization batches",
	})
	m.WriteJournalProjectionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_write_journal_projection_duration_seconds",
		Help:    "MySQL materialization latency for write-journal batches",
		Buckets: prometheus.DefBuckets,
	})
	m.WriteJournalProjectionErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_write_journal_projection_errors_total",
		Help: "Write-journal reads or MySQL materialization failures",
	})

	m.DeltaBatches = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "farm_delta_broadcast_batches_total",
		Help: "FarmDelta PushBatch count (one per Gateway fan-out job; local RoomHub counts as 1)",
	})
	m.DeltaTargets = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_delta_broadcast_targets",
		Help:    "Target connection count per FarmDelta publish (sum of conn_ids across all batches in that publish)",
		Buckets: []float64{0, 1, 2, 4, 8, 16, 32, 64, 128},
	})
	m.DeltaEncodeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_delta_encode_duration_seconds",
		Help:    "FarmDelta Envelope encode latency (once per publish)",
		Buckets: prometheus.DefBuckets,
	})
	m.DeltaPushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "farm_delta_push_duration_seconds",
		Help:    "FarmDelta push/fan-out latency after encode (covers all batches in the publish)",
		Buckets: prometheus.DefBuckets,
	})

	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
		m.HTTPRequests, m.HTTPDuration,
		m.WSConnections, m.WSRequests, m.WSDuration,
		m.WSDisconnects, m.WSHandshakeErrors, m.WSSessionReplacements,
		m.WSWriteQueueDepth, m.WSWriteQueueFull, m.WSWriteFailures,
		m.ActorResident, m.ActorMailboxDepth, m.ActorDoBusy,
		m.ActorLoadDuration, m.ActorLoadErrors,
		m.ActorSaveDuration, m.ActorSaveErrors,
		m.CommitBatches, m.CommitFarms, m.CommitRequests,
		m.WriteJournalAppends, m.WriteJournalAppendRecords,
		m.WriteJournalAppendDuration, m.WriteJournalAppendErrors,
		m.WriteJournalProjectionBatches, m.WriteJournalProjectionRecords,
		m.WriteJournalProjectionDuration, m.WriteJournalProjectionErrors,
		m.DeltaBatches, m.DeltaTargets, m.DeltaEncodeDuration, m.DeltaPushDuration,
	)
	return m
}

// ObserveWriteJournalAppend records the foreground durability boundary.
func (m *Metrics) ObserveWriteJournalAppend(duration time.Duration, records int, err error) {
	if m == nil {
		return
	}
	m.WriteJournalAppends.Inc()
	m.WriteJournalAppendRecords.Add(float64(records))
	m.WriteJournalAppendDuration.Observe(duration.Seconds())
	if err != nil {
		m.WriteJournalAppendErrors.Inc()
	}
}

// ObserveWriteJournalProjection records one background materialization batch.
func (m *Metrics) ObserveWriteJournalProjection(duration time.Duration, records int, err error) {
	if m == nil {
		return
	}
	m.WriteJournalProjectionBatches.Inc()
	m.WriteJournalProjectionRecords.Add(float64(records))
	m.WriteJournalProjectionDuration.Observe(duration.Seconds())
	if err != nil {
		m.WriteJournalProjectionErrors.Inc()
	}
}

func (m *Metrics) ObserveWriteJournalProjectionError() {
	if m != nil {
		m.WriteJournalProjectionErrors.Inc()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack 透传给底层 ResponseWriter，否则 WebSocket Upgrade 会失败。
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("obs: ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// NormalizeRoute collapses request paths into low-cardinality route templates.
// Never returns raw paths that embed invite tokens or other high-cardinality suffixes.
func NormalizeRoute(path string) string {
	switch {
	case path == "/api/register":
		return "/api/register"
	case path == "/api/login":
		return "/api/login"
	case path == "/ws":
		return "/ws"
	case path == "/api/debug/advance":
		return "/api/debug/advance"
	case strings.HasPrefix(path, "/i/"):
		return "/i/"
	default:
		return "other"
	}
}

// InstrumentHTTP wraps a business handler with request count/latency metrics.
// route must be a low-cardinality template (e.g. /api/login), never the raw URL path.
func (m *Metrics) InstrumentHTTP(route string, next http.Handler) http.Handler {
	if m == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		code := strconv.Itoa(rec.code)
		m.HTTPRequests.WithLabelValues(r.Method, route, code).Inc()
		m.HTTPDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// InstrumentHandler wraps an entire business mux, labeling by NormalizeRoute(path).
func (m *Metrics) InstrumentHandler(next http.Handler) http.Handler {
	if m == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := NormalizeRoute(r.URL.Path)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		code := strconv.Itoa(rec.code)
		m.HTTPRequests.WithLabelValues(r.Method, route, code).Inc()
		m.HTTPDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// ObserveWSRequest records one WebSocket command handling sample.
// cmd/code are protocol integers (bounded cardinality); never uid/conn_id.
func (m *Metrics) ObserveWSRequest(cmd, code uint32, d time.Duration) {
	if m == nil {
		return
	}
	if code == 0 {
		if counter, ok := m.wsOKRequests[cmd]; ok {
			counter.Inc()
			m.wsDurations[cmd].Observe(d.Seconds())
			return
		}
	}
	cmdLabel := strconv.FormatUint(uint64(cmd), 10)
	codeLabel := strconv.FormatUint(uint64(code), 10)
	m.WSRequests.WithLabelValues(cmdLabel, codeLabel).Inc()
	m.WSDuration.WithLabelValues(cmdLabel).Observe(d.Seconds())
}

// ObserveWSDisconnect records exactly one bounded reason per accepted socket.
func (m *Metrics) ObserveWSDisconnect(reason string) {
	if m == nil {
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	m.WSDisconnects.WithLabelValues(reason).Inc()
}

func (m *Metrics) ObserveWSHandshakeError(code uint32) {
	if m == nil || code == 0 {
		return
	}
	m.WSHandshakeErrors.WithLabelValues(strconv.FormatUint(uint64(code), 10)).Inc()
}

func (m *Metrics) ObserveWSSessionReplacement() {
	if m != nil {
		m.WSSessionReplacements.Inc()
	}
}

func (m *Metrics) ObserveWSWriteQueueDepth(depth int) {
	if m != nil && depth >= 0 {
		m.WSWriteQueueDepth.Observe(float64(depth))
	}
}

func (m *Metrics) ObserveWSWriteQueueFull() {
	if m != nil {
		m.WSWriteQueueFull.Inc()
	}
}

func (m *Metrics) ObserveWSWriteFailure(path string) {
	if m == nil {
		return
	}
	if path != "push" {
		path = "response"
	}
	m.WSWriteFailures.WithLabelValues(path).Inc()
}

var knownWSCommands = [...]uint32{
	100, 102,
	200, 202, 204, 206, 208, 210, 212, 214, 216, 218, 220, 222,
	302, 304,
	400, 402, 404, 406, 408, 410, 412, 414, 416, 418,
	500, 502, 504,
	600, 602, 604, 606, 608, 610, 612, 614, 616,
}

// ObserveMailboxDepth records a mailbox depth sample.
func (m *Metrics) ObserveMailboxDepth(depth int) {
	if m == nil {
		return
	}
	m.ActorMailboxDepth.Observe(float64(depth))
}

// ObserveActorLoad records load latency and optional failure.
func (m *Metrics) ObserveActorLoad(d time.Duration, err error) {
	if m == nil {
		return
	}
	m.ActorLoadDuration.Observe(d.Seconds())
	if err != nil {
		m.ActorLoadErrors.Inc()
	}
}

// ObserveActorSave records save/flush latency and optional failure.
func (m *Metrics) ObserveActorSave(d time.Duration, err error) {
	if m == nil {
		return
	}
	m.ActorSaveDuration.Observe(d.Seconds())
	if err != nil {
		m.ActorSaveErrors.Inc()
	}
}

// ObserveCommitBatch records transaction packing and same-UID coalescing.
// requests/uids is the logical coalescing ratio; uids/batch is cross-farm
// transaction packing. Neither label carries player identifiers.
func (m *Metrics) ObserveCommitBatch(uids, requests int) {
	if m == nil || uids <= 0 || requests <= 0 {
		return
	}
	m.CommitBatches.Inc()
	m.CommitFarms.Add(float64(uids))
	m.CommitRequests.Add(float64(requests))
}

// ObserveDeltaBroadcast records FarmDelta fan-out for one publish.
// batches is the number of PushBatch deliveries (len(gateway groups); RoomHub = 1).
// targets is the total connection count across those batches.
// Zero batches (no targets) is a no-op so empty rooms do not invent counters.
func (m *Metrics) ObserveDeltaBroadcast(batches, targets int, encode, push time.Duration) {
	if m == nil || batches <= 0 {
		return
	}
	m.DeltaBatches.Add(float64(batches))
	m.DeltaTargets.Observe(float64(targets))
	m.DeltaEncodeDuration.Observe(encode.Seconds())
	m.DeltaPushDuration.Observe(push.Seconds())
}
