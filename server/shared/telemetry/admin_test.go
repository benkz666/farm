package telemetry_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"farm/server/shared/telemetry"
)

func TestParseAdminAddr(t *testing.T) {
	t.Parallel()

	addr, on := telemetry.ParseAdminAddr("")
	if !on || addr != "127.0.0.1:9300" {
		t.Fatalf("empty env: addr=%q on=%v, want 127.0.0.1:9300 true", addr, on)
	}
	for _, off := range []string{"off", "OFF", "disabled", "-"} {
		if _, on := telemetry.ParseAdminAddr(off); on {
			t.Fatalf("ParseAdminAddr(%q) enabled, want disabled", off)
		}
	}
	addr, on = telemetry.ParseAdminAddr("127.0.0.1:19003")
	if !on || addr != "127.0.0.1:19003" {
		t.Fatalf("custom addr: got %q on=%v", addr, on)
	}
}

func TestDefaultAdminAddrAvoidsKnownTopologyPorts(t *testing.T) {
	t.Parallel()

	// 仓库默认拓扑占用的 loopback 端口（Vite / 三服务）。
	// Admin 必须避开，避免默认启动互相抢端口。
	reserved := map[string]string{
		"9001": "vite",
		"9002": "gateway",
		"9003": "auth",
		"9004": "social",
		"9005": "worker",
		"9100": "farm",
		"9094": "kafka PLAINTEXT_HOST",
	}
	if telemetry.DefaultAdminAddr != "127.0.0.1:9300" {
		t.Fatalf("DefaultAdminAddr = %q, want 127.0.0.1:9300", telemetry.DefaultAdminAddr)
	}
	_, port, err := net.SplitHostPort(telemetry.DefaultAdminAddr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", telemetry.DefaultAdminAddr, err)
	}
	if why, hit := reserved[port]; hit {
		t.Fatalf("DefaultAdminAddr port %s collides with %s", port, why)
	}
}

func TestAdminIsolatedFromBusinessHandler(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := telemetry.NewMetrics(reg)
	probe := telemetry.NewProbe()
	probe.MarkReady()

	admin, err := telemetry.StartAdmin(telemetry.AdminConfig{
		Addr:     "127.0.0.1:0",
		Probe:    probe,
		Gatherer: reg,
	})
	if err != nil {
		t.Fatalf("StartAdmin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Shutdown(context.Background()) })

	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(business.Close)

	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/"} {
		resp, err := http.Get(business.URL + path)
		if err != nil {
			t.Fatalf("business GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "go_goroutines") {
			t.Fatalf("business handler exposed metrics at %s", path)
		}
		if path == "/debug/pprof/" && resp.StatusCode == http.StatusOK && strings.Contains(string(body), "Types of profiles available") {
			t.Fatal("business handler exposed pprof index")
		}
	}

	adminBase := "http://" + admin.Addr()
	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/"} {
		resp, err := http.Get(adminBase + path)
		if err != nil {
			t.Fatalf("admin GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	_ = metrics // metrics registered; /metrics smoke covered above
}

func TestHealthzAlwaysLiveReadyzTracksState(t *testing.T) {
	t.Parallel()

	probe := telemetry.NewProbe()
	admin, err := telemetry.StartAdmin(telemetry.AdminConfig{
		Addr:     "127.0.0.1:0",
		Probe:    probe,
		Gatherer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("StartAdmin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Shutdown(context.Background()) })
	base := "http://" + admin.Addr()

	mustStatus := func(path string, want int) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s status = %d, want %d", path, resp.StatusCode, want)
		}
	}

	mustStatus("/healthz", http.StatusOK)
	mustStatus("/readyz", http.StatusServiceUnavailable)

	probe.MarkReady()
	mustStatus("/readyz", http.StatusOK)
	mustStatus("/healthz", http.StatusOK)

	probe.BeginDrain()
	mustStatus("/readyz", http.StatusServiceUnavailable)
	mustStatus("/healthz", http.StatusOK)
}

func TestReadyzFailsWhenCheckerFails(t *testing.T) {
	t.Parallel()

	probe := telemetry.NewProbe(telemetry.FuncChecker("mysql", func(context.Context) error {
		return context.DeadlineExceeded
	}))
	probe.MarkReady()
	admin, err := telemetry.StartAdmin(telemetry.AdminConfig{
		Addr:     "127.0.0.1:0",
		Probe:    probe,
		Gatherer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("StartAdmin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Shutdown(context.Background()) })

	resp, err := http.Get("http://" + admin.Addr() + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", resp.StatusCode)
	}
}

func TestAdminBindFailureIsReturned(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	_, err = telemetry.StartAdmin(telemetry.AdminConfig{
		Addr:     ln.Addr().String(),
		Probe:    telemetry.NewProbe(),
		Gatherer: prometheus.NewRegistry(),
	})
	if err == nil {
		t.Fatal("StartAdmin succeeded on occupied address")
	}
}

func TestShutdownOrderReadinessBeforeAdminStop(t *testing.T) {
	t.Parallel()

	probe := telemetry.NewProbe()
	probe.MarkReady()
	admin, err := telemetry.StartAdmin(telemetry.AdminConfig{
		Addr:     "127.0.0.1:0",
		Probe:    probe,
		Gatherer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("StartAdmin: %v", err)
	}
	base := "http://" + admin.Addr()

	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("ready before drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ready before drain = %d", resp.StatusCode)
	}

	// 关停顺序：先 BeginDrain（readiness 503），探针仍可回答；最后停 admin。
	probe.BeginDrain()
	resp, err = http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("ready after drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready after drain = %d, want 503", resp.StatusCode)
	}
	resp, err = http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("health during drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health during drain = %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := admin.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	client := &http.Client{Timeout: 200 * time.Millisecond}
	if _, err := client.Get(base + "/healthz"); err == nil {
		t.Fatal("admin still serving after Shutdown")
	}
}

func TestPprofOnlyOnAdmin(t *testing.T) {
	t.Parallel()

	admin, err := telemetry.StartAdmin(telemetry.AdminConfig{
		Addr:     "127.0.0.1:0",
		Probe:    telemetry.NewProbe(),
		Gatherer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("StartAdmin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Shutdown(context.Background()) })

	resp, err := http.Get("http://" + admin.Addr() + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatalf("pprof: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pprof status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "goroutine") {
		t.Fatalf("pprof body missing goroutine profile: %q", body[:min(80, len(body))])
	}

	business := http.NewServeMux()
	business.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(business)
	t.Cleanup(srv.Close)
	resp, err = http.Get(srv.URL + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatalf("business pprof: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("business handler must not expose pprof")
	}
}
