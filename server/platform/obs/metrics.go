package obs

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds process metrics registered on an injectable registry.
// Never uses prometheus.DefaultRegisterer (avoids duplicate-registration panics in tests).
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec

	WSConnections prometheus.Gauge
	WSRequests    *prometheus.CounterVec
	WSDuration    *prometheus.HistogramVec

	ActorResident     prometheus.Gauge
	ActorMailboxDepth prometheus.Histogram
	ActorDoBusy       prometheus.Counter
	ActorLoadDuration prometheus.Histogram
	ActorLoadErrors   prometheus.Counter
	ActorSaveDuration prometheus.Histogram
	ActorSaveErrors   prometheus.Counter

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
		m.HTTPRequests, m.HTTPDuration,
		m.WSConnections, m.WSRequests, m.WSDuration,
		m.ActorResident, m.ActorMailboxDepth, m.ActorDoBusy,
		m.ActorLoadDuration, m.ActorLoadErrors,
		m.ActorSaveDuration, m.ActorSaveErrors,
		m.DeltaBatches, m.DeltaTargets, m.DeltaEncodeDuration, m.DeltaPushDuration,
	)
	return m
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
	case path == "/internal/v1/cmd":
		return "/internal/v1/cmd"
	case path == "/internal/v1/push/farm-delta":
		return "/internal/v1/push/farm-delta"
	case path == "/internal/v1/push/farm-delta-batch":
		return "/internal/v1/push/farm-delta-batch"
	case path == "/internal/v1/push/player-delta":
		return "/internal/v1/push/player-delta"
	case path == "/api/debug/advance":
		return "/api/debug/advance"
	case path == "/internal/v1/debug/advance":
		return "/internal/v1/debug/advance"
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
	cmdLabel := strconv.FormatUint(uint64(cmd), 10)
	codeLabel := strconv.FormatUint(uint64(code), 10)
	m.WSRequests.WithLabelValues(cmdLabel, codeLabel).Inc()
	m.WSDuration.WithLabelValues(cmdLabel).Observe(d.Seconds())
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
