package servicehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"farm/server/platform/obs"
)

const shutdownTimeout = 10 * time.Second

// Host 描述一个独立服务进程。Handler 只包含该服务拥有的路由。
type Host struct {
	Config         Config
	Handler        http.Handler
	Checker        obs.Checker
	Metrics        *obs.Metrics
	BeforeShutdown func(context.Context) error
}

// Run 启动业务监听、探针与指标，并按固定顺序优雅关停。
func (host Host) Run(ctx context.Context) error {
	if host.Handler == nil {
		return errors.New("servicehost: handler must not be nil")
	}
	obs.SetDefault(obs.NewLogger(os.Stderr, slog.LevelInfo))
	metrics := host.Metrics
	if metrics == nil {
		metrics = obs.NewMetrics(nil)
	}
	checker := host.Checker
	if checker == nil {
		checker = obs.FuncChecker("process", func(context.Context) error { return nil })
	}
	probe := obs.NewProbe(checker)
	adminAddr, adminEnabled := obs.ParseAdminAddr(host.Config.AdminAddr)
	var admin *obs.Admin
	var err error
	if adminEnabled {
		admin, err = obs.StartAdmin(obs.AdminConfig{Addr: adminAddr, Probe: probe, Gatherer: metrics.Registry})
		if err != nil {
			return fmt.Errorf("%s: start admin listener: %w", host.Config.Name, err)
		}
	}

	server := &http.Server{
		Addr:              host.Config.HTTPAddr,
		Handler:           metrics.InstrumentHandler(host.Handler),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverError := make(chan error, 1)
	go func() { serverError <- server.ListenAndServe() }()
	probe.MarkReady()
	obs.L().Info("service listening", "component", host.Config.Name, "addr", host.Config.HTTPAddr)

	select {
	case err := <-serverError:
		probe.BeginDrain()
		if host.BeforeShutdown != nil {
			drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			drainError := host.BeforeShutdown(drainCtx)
			cancel()
			if drainError != nil {
				shutdownAdmin(admin)
				return fmt.Errorf("%s: drain state after server failure: %w", host.Config.Name, drainError)
			}
		}
		shutdownAdmin(admin)
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%s: serve HTTP: %w", host.Config.Name, err)
		}
		return nil
	case <-ctx.Done():
		probe.BeginDrain()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		httpError := server.Shutdown(shutdownCtx)
		cancel()
		serveError := <-serverError
		if host.BeforeShutdown != nil {
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
			drainError := host.BeforeShutdown(drainCtx)
			drainCancel()
			if drainError != nil {
				shutdownAdmin(admin)
				return fmt.Errorf("%s: drain state: %w", host.Config.Name, drainError)
			}
		}
		shutdownAdmin(admin)
		if httpError != nil {
			return fmt.Errorf("%s: shutdown HTTP: %w", host.Config.Name, httpError)
		}
		if !errors.Is(serveError, http.ErrServerClosed) {
			return fmt.Errorf("%s: serve HTTP after shutdown: %w", host.Config.Name, serveError)
		}
		return nil
	}
}

func shutdownAdmin(admin *obs.Admin) {
	if admin == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := admin.Shutdown(ctx); err != nil {
		obs.L().Error("shutdown admin failed", "component", "servicehost", "err", err.Error())
	}
}
