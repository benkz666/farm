// Package telemetry provides process health probes, Prometheus metrics, admin HTTP
// endpoints (including pprof), and a shared slog logger baseline.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultAdminAddr 是未指定地址时的通用回退值。
// 三个服务的正式默认值由各自入口设置。
const DefaultAdminAddr = "127.0.0.1:9300"

// ParseAdminAddr 把配置值解析为监听地址。
// Unset/empty → default loopback; off/disabled/- → disabled.
func ParseAdminAddr(env string) (addr string, enabled bool) {
	v := strings.TrimSpace(env)
	if v == "" {
		return DefaultAdminAddr, true
	}
	switch strings.ToLower(v) {
	case "off", "disabled", "-":
		return "", false
	default:
		return v, true
	}
}

// Checker is a readiness dependency (MySQL, Redis, …).
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

type namedChecker struct {
	name string
	fn   func(context.Context) error
}

func (c namedChecker) Name() string                    { return c.name }
func (c namedChecker) Check(ctx context.Context) error { return c.fn(ctx) }

// FuncChecker wraps a named check function.
func FuncChecker(name string, fn func(context.Context) error) Checker {
	return namedChecker{name: name, fn: fn}
}

// Probe tracks liveness vs readiness. /healthz is always live while the
// process runs; /readyz requires MarkReady, no BeginDrain, and all checkers OK.
type Probe struct {
	mu       sync.RWMutex
	ready    bool
	draining bool
	checks   []Checker
}

// NewProbe constructs a probe with optional dependency checkers.
func NewProbe(checks ...Checker) *Probe {
	return &Probe{checks: append([]Checker(nil), checks...)}
}

// MarkReady signals startup finished and the process may receive traffic.
func (p *Probe) MarkReady() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.ready = true
	p.mu.Unlock()
}

// BeginDrain flips readiness false immediately (load balancers stop routing).
func (p *Probe) BeginDrain() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.draining = true
	p.mu.Unlock()
}

// Ready reports whether /readyz should return 200.
func (p *Probe) Ready(ctx context.Context) error {
	if p == nil {
		return errors.New("probe nil")
	}
	p.mu.RLock()
	ready, draining := p.ready, p.draining
	checks := p.checks
	p.mu.RUnlock()
	if draining {
		return errors.New("draining")
	}
	if !ready {
		return errors.New("not ready")
	}
	for _, c := range checks {
		if c == nil {
			continue
		}
		if err := c.Check(ctx); err != nil {
			return fmt.Errorf("%s: %w", c.Name(), err)
		}
	}
	return nil
}

// AdminConfig configures the isolated admin HTTP listener.
type AdminConfig struct {
	Addr     string
	Probe    *Probe
	Gatherer prometheus.Gatherer
}

// Admin is the loopback (by default) ops listener: health, metrics, pprof.
type Admin struct {
	probe  *Probe
	server *http.Server
	ln     net.Listener
	addr   string
}

// StartAdmin binds immediately so bind failures surface at startup, then serves in the background.
func StartAdmin(cfg AdminConfig) (*Admin, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("obs: admin addr is empty")
	}
	probe := cfg.Probe
	if probe == nil {
		probe = NewProbe()
	}
	gatherer := cfg.Gatherer
	if gatherer == nil {
		gatherer = prometheus.NewRegistry()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := probe.Ready(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{Registry: nil}))
	registerPprof(mux)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("obs: listen admin %s: %w", cfg.Addr, err)
	}
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	admin := &Admin{probe: probe, server: server, ln: ln, addr: ln.Addr().String()}
	go func() { _ = server.Serve(ln) }()
	return admin, nil
}

// Addr returns the actual bound address (useful when Addr was :0).
func (a *Admin) Addr() string {
	if a == nil {
		return ""
	}
	return a.addr
}

// Shutdown stops the admin listener. Call after readiness is already false and
// after business HTTP/Actor shutdown so probes never block process exit.
func (a *Admin) Shutdown(ctx context.Context) error {
	if a == nil || a.server == nil {
		return nil
	}
	return a.server.Shutdown(ctx)
}

func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
}
