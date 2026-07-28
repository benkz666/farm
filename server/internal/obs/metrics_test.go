package obs_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"farm/server/internal/obs"
)

func TestMetricsRegistryIsInjectable(t *testing.T) {
	t.Parallel()

	reg1 := prometheus.NewRegistry()
	reg2 := prometheus.NewRegistry()
	m1 := obs.NewMetrics(reg1)
	m2 := obs.NewMetrics(reg2)
	if m1 == nil || m2 == nil {
		t.Fatal("NewMetrics returned nil")
	}
	// 两次注册不得 panic（禁止依赖 prometheus.DefaultRegisterer）。
	m1.HTTPRequests.WithLabelValues("GET", "/api/login", "200").Inc()
	m2.HTTPRequests.WithLabelValues("GET", "/api/login", "200").Inc()
}

func TestHTTPMiddlewareRecordsRouteNotRawPath(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := obs.NewMetrics(reg)
	handler := m.InstrumentHTTP("/api/login", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/login?x=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("POST", "/api/login", "401")); got != 1 {
		t.Fatalf("http requests = %v, want 1", got)
	}

	body := gatherText(t, reg)
	if strings.Contains(body, "uid") || strings.Contains(body, "conn_id") || strings.Contains(body, "req_id") {
		t.Fatalf("metrics leaked high-cardinality labels:\n%s", body)
	}
	if strings.Contains(body, "/api/login?x=1") {
		t.Fatal("metrics used raw URL path as label")
	}
}

func TestWSAndActorAndDeltaMetrics(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := obs.NewMetrics(reg)

	m.WSConnections.Inc()
	m.ObserveWSRequest(200, 0, 12*time.Millisecond)
	m.ActorResident.Set(3)
	m.ObserveMailboxDepth(2)
	m.ActorDoBusy.Inc()
	m.ObserveActorLoad(5*time.Millisecond, nil)
	m.ObserveActorSave(8*time.Millisecond, io.EOF)
	m.ObserveDeltaBroadcast(1, 4, 3*time.Millisecond, 7*time.Millisecond)

	body := gatherText(t, reg)
	for _, name := range []string{
		"farm_ws_connections",
		"farm_ws_requests_total",
		"farm_actor_resident",
		"farm_actor_mailbox_depth",
		"farm_actor_do_busy_total",
		"farm_actor_load_duration_seconds",
		"farm_actor_save_errors_total",
		"farm_delta_broadcast_batches_total",
		"farm_delta_broadcast_targets",
		"farm_delta_encode_duration_seconds",
		"farm_delta_push_duration_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("missing metric %s in:\n%s", name, body)
		}
	}
	if strings.Contains(body, `uid="`) || strings.Contains(body, `conn_id="`) {
		t.Fatalf("high-cardinality labels present:\n%s", body)
	}
}

func TestObserveDeltaBroadcastAddsBatchCount(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := obs.NewMetrics(reg)

	m.ObserveDeltaBroadcast(2, 3, time.Millisecond, 2*time.Millisecond)
	if got := testutil.ToFloat64(m.DeltaBatches); got != 2 {
		t.Fatalf("batches after Observe(2,3) = %v, want 2", got)
	}

	m.ObserveDeltaBroadcast(0, 0, time.Millisecond, time.Millisecond)
	if got := testutil.ToFloat64(m.DeltaBatches); got != 2 {
		t.Fatalf("zero-batch observe mutated counter to %v, want 2", got)
	}

	m.ObserveDeltaBroadcast(1, 1, time.Millisecond, time.Millisecond)
	if got := testutil.ToFloat64(m.DeltaBatches); got != 3 {
		t.Fatalf("batches after Observe(1,1) = %v, want 3", got)
	}
}

func TestDefaultRegistryNotUsed(t *testing.T) {
	t.Parallel()

	// 连续创建多个 Metrics 不得因 DefaultRegisterer 重复注册而 panic。
	for i := 0; i < 3; i++ {
		_ = obs.NewMetrics(nil)
	}
}

func gatherText(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var b strings.Builder
	for _, mf := range mfs {
		b.WriteString(mf.GetName())
		b.WriteByte('\n')
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				b.WriteString(lp.GetName())
				b.WriteByte('=')
				b.WriteString(lp.GetValue())
				b.WriteByte(' ')
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}
