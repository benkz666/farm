package servicehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"farm/server/shared/grpcx"
	"farm/server/shared/telemetry"

	"google.golang.org/grpc"
)

const shutdownTimeout = 10 * time.Second

// GRPC describes an internal gRPC listener owned by one process.
type GRPC struct {
	Addr     string
	Register func(*grpc.Server)
}

// Host 描述一个独立服务进程。Handler 只包含该服务拥有的路由。
type Host struct {
	Config         Config
	Handler        http.Handler
	Checker        telemetry.Checker
	Metrics        *telemetry.Metrics
	BeforeShutdown func(context.Context) error
	GRPC           *GRPC
	GRPCPool       *grpcx.Pool
	GRPCReady      func(context.Context) error
}

// Run 启动业务监听、探针与指标，并按固定顺序优雅关停。
func (host Host) Run(ctx context.Context) error {
	if host.Handler == nil {
		return errors.New("servicehost: handler must not be nil")
	}
	telemetry.SetDefault(telemetry.NewLogger(os.Stderr, slog.LevelInfo))
	metrics := host.Metrics
	if metrics == nil {
		metrics = telemetry.NewMetrics(nil)
	}
	checker := host.Checker
	if checker == nil {
		checker = telemetry.FuncChecker("process", func(context.Context) error { return nil })
	}
	if host.GRPCReady != nil {
		checker = withGRPCReady(checker, host.GRPCReady)
	}
	probe := telemetry.NewProbe(checker)
	adminAddr, adminEnabled := telemetry.ParseAdminAddr(host.Config.AdminAddr)
	var admin *telemetry.Admin
	var err error
	if adminEnabled {
		admin, err = telemetry.StartAdmin(telemetry.AdminConfig{Addr: adminAddr, Probe: probe, Gatherer: metrics.Registry})
		if err != nil {
			return fmt.Errorf("%s: start admin listener: %w", host.Config.Name, err)
		}
	}

	var (
		grpcServer   *grpc.Server
		grpcListener net.Listener
		grpcServeErr chan error
	)
	if host.GRPC != nil && host.GRPC.Addr != "" {
		grpcServer = grpc.NewServer(grpcx.ServerOptions([]byte(host.Config.InternalToken))...)
		if host.GRPC.Register != nil {
			host.GRPC.Register(grpcServer)
		}
		grpcListener, err = net.Listen("tcp", host.GRPC.Addr)
		if err != nil {
			shutdownAdmin(admin)
			return fmt.Errorf("%s: listen gRPC %s: %w", host.Config.Name, host.GRPC.Addr, err)
		}
		grpcServeErr = make(chan error, 1)
		go func() { grpcServeErr <- grpcServer.Serve(grpcListener) }()
	}

	server := &http.Server{
		Addr:              host.Config.HTTPAddr,
		Handler:           metrics.InstrumentHandler(host.Handler),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverError := make(chan error, 1)
	go func() { serverError <- server.ListenAndServe() }()
	probe.MarkReady()
	telemetry.L().Info("service listening",
		"component", host.Config.Name,
		"addr", host.Config.HTTPAddr,
		"grpc_addr", host.Config.GRPCAddr,
	)

	var grpcErrCh <-chan error
	if grpcServeErr != nil {
		grpcErrCh = grpcServeErr
	}

	select {
	case err := <-serverError:
		probe.BeginDrain()
		if shutdownErr := host.shutdown(ctx, server, grpcServer, admin); shutdownErr != nil {
			return shutdownErr
		}
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%s: serve HTTP: %w", host.Config.Name, err)
		}
		return waitGRPC(host, grpcServeErr)
	case err := <-grpcErrCh:
		probe.BeginDrain()
		if shutdownErr := host.shutdown(ctx, server, grpcServer, admin); shutdownErr != nil {
			return shutdownErr
		}
		return fmt.Errorf("%s: serve gRPC: %w", host.Config.Name, err)
	case <-ctx.Done():
		probe.BeginDrain()
		if err := host.shutdown(ctx, server, grpcServer, admin); err != nil {
			return err
		}
		serveError := <-serverError
		if grpcErr := waitGRPC(host, grpcServeErr); grpcErr != nil {
			return grpcErr
		}
		if !errors.Is(serveError, http.ErrServerClosed) {
			return fmt.Errorf("%s: serve HTTP after shutdown: %w", host.Config.Name, serveError)
		}
		return nil
	}
}

func withGRPCReady(base telemetry.Checker, ready func(context.Context) error) telemetry.Checker {
	return telemetry.FuncChecker("dependencies", func(ctx context.Context) error {
		if err := base.Check(ctx); err != nil {
			return err
		}
		return ready(ctx)
	})
}

func (host Host) shutdown(ctx context.Context, server *http.Server, grpcServer *grpc.Server, admin *telemetry.Admin) error {
	httpStopped := make(chan error, 1)
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		httpStopped <- server.Shutdown(shutdownCtx)
	}()
	grpcStopped := make(chan error, 1)
	go func() {
		grpcStopped <- stopGRPCWithTimeout(grpcServer, shutdownTimeout)
	}()

	var drainError error
	if host.BeforeShutdown != nil {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
		drainError = host.BeforeShutdown(drainCtx)
		drainCancel()
	}
	httpError := <-httpStopped
	grpcStopError := <-grpcStopped
	shutdownAdmin(admin)
	if host.GRPCPool != nil {
		if err := host.GRPCPool.Close(); err != nil {
			return fmt.Errorf("%s: close gRPC pool: %w", host.Config.Name, err)
		}
	}
	if drainError != nil {
		return fmt.Errorf("%s: drain state: %w", host.Config.Name, drainError)
	}
	if grpcStopError != nil {
		return fmt.Errorf("%s: stop gRPC: %w", host.Config.Name, grpcStopError)
	}
	if httpError != nil {
		return fmt.Errorf("%s: shutdown HTTP: %w", host.Config.Name, httpError)
	}
	return nil
}

func stopGRPCWithTimeout(server *grpc.Server, timeout time.Duration) error {
	if server == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = shutdownTimeout
	}
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-stopped:
		return nil
	case <-timer.C:
		server.Stop()
		<-stopped
		return errors.New("graceful stop timed out; forced stop")
	}
}

func waitGRPC(host Host, grpcServeErr <-chan error) error {
	if grpcServeErr == nil {
		return nil
	}
	err := <-grpcServeErr
	if err != nil {
		return fmt.Errorf("%s: serve gRPC after shutdown: %w", host.Config.Name, err)
	}
	return nil
}

func shutdownAdmin(admin *telemetry.Admin) {
	if admin == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := admin.Shutdown(ctx); err != nil {
		telemetry.L().Error("shutdown admin failed", "component", "servicehost", "err", err.Error())
	}
}
