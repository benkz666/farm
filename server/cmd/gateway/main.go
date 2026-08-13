// gateway 是客户端唯一入口，负责 HTTP/WS 连接、鉴权结果转发、限流与分片路由。
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"farm/server/auth"
	"farm/server/gateway"
	"farm/server/gateway/apidocs"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/presence"
	"farm/server/shared/servicehost"
	"farm/server/shared/telemetry"
	"google.golang.org/grpc"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := servicehost.LoadConfig("gateway", ":9002", "127.0.0.1:9302")
	if err != nil {
		return err
	}
	if config.GRPCAddr == "" {
		config.GRPCAddr = servicehost.Getenv("FARM_GATEWAY_GRPC_ADDR", ":9202")
	}
	instanceID := servicehost.Getenv("FARM_GATEWAY_INSTANCE_ID", "gateway-0")
	advertiseTarget, err := gatewayAdvertiseTarget(config.GRPCAddr, os.Getenv)
	if err != nil {
		return err
	}
	routePath := servicehost.Getenv("FARM_ROUTE_TABLE", "deploy/route-table.local.json")
	routes, err := servicehost.LoadRouteTable(routePath)
	if err != nil {
		return err
	}
	farmTargets, err := servicehost.ParseGRPCTargetMap(
		"FARM_FARM_GRPC_TARGETS",
		servicehost.Getenv("FARM_FARM_GRPC_TARGETS", `{"farm-0":"127.0.0.1:9210"}`),
	)
	if err != nil {
		return err
	}
	gatewayTargets, err := servicehost.ParseGRPCTargetMap(
		"FARM_GATEWAY_GRPC_TARGETS",
		servicehost.Getenv("FARM_GATEWAY_GRPC_TARGETS", `{"gateway-0":"127.0.0.1:9202"}`),
	)
	if err != nil {
		return err
	}
	if gatewayTargets[instanceID] == "" && advertiseTarget == "" {
		return fmt.Errorf("gateway: FARM_GATEWAY_GRPC_TARGETS has no entry for %q", instanceID)
	}
	socialTarget, err := servicehost.RequiredGRPCTarget(
		"FARM_SOCIAL_GRPC_TARGET",
		"127.0.0.1:9204",
	)
	if err != nil {
		return err
	}
	timeProfile := strings.ToLower(servicehost.Getenv("FARM_TIME_PROFILE", gameconfig.TimeProfileDemo))
	if !gameconfig.ValidTimeProfile(timeProfile) {
		return fmt.Errorf("gateway: unsupported FARM_TIME_PROFILE %q", timeProfile)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	storage, closeStorage, err := servicehost.OpenStorage(ctx, config)
	if err != nil {
		return err
	}
	defer closeStorage()
	gatewayDirectory := storage.GatewayDirectory()
	instanceTTL := presence.DefaultGatewayInstanceTTL
	if advertiseTarget != "" {
		instanceTTL, err = positiveDurationSetting(
			"FARM_GATEWAY_INSTANCE_TTL",
			presence.DefaultGatewayInstanceTTL,
		)
		if err != nil {
			return err
		}
		if err := gatewayDirectory.Register(ctx, instanceID, advertiseTarget, instanceTTL); err != nil {
			return fmt.Errorf("gateway: register dynamic instance: %w", err)
		}
		go renewGatewayRegistration(ctx, gatewayDirectory, instanceID, advertiseTarget, instanceTTL)
	}

	metrics := telemetry.NewMetrics(nil)
	timeProfiles := gameconfig.NewTimeProfileSwitch(timeProfile)
	authService := auth.New(storage, storage)
	grpcPool := grpcx.NewPool(config.InternalToken)
	socialClient := gateway.NewSocialClient(grpcPool, socialTarget)
	options := []gateway.Option{
		gateway.WithFarmRPC(gateway.NewFarmClient(grpcPool, farmTargets), routes),
		gateway.WithSocialRPC(socialClient),
		gateway.WithConnectionRegistry(storage.ConnectionRegistry(), instanceID),
		gateway.WithSessionKickPusher(gateway.NewPeerSessionKickClient(grpcPool, gatewayTargets, gatewayDirectory)),
		gateway.WithDebugTimeFanout(grpcPool, farmTargets, gatewayTargets, instanceID, gatewayDirectory),
		gateway.WithMetrics(metrics),
		gateway.WithTimeProfileSwitch(timeProfiles),
	}
	if servicehost.Getenv("FARM_DISABLE_WS_RATE_LIMIT", "0") == "1" {
		options = append(options, gateway.WithWSRateLimitDisabled())
	}
	if apiDocsEnabled(config.Environment, os.Getenv) {
		options = append(options, gateway.WithAPIDocs(apidocs.Handler()))
	}
	transport := gateway.New(authService, storage, options...)
	if servicehost.Getenv("FARM_ALLOW_DEBUG_TIME", "0") == "1" {
		transport.EnableDebugTime()
	}

	return (servicehost.Host{
		Config:  config,
		Handler: transport.Handler(),
		Checker: telemetry.FuncChecker("storage", storage.Ping),
		Metrics: metrics,
		GRPC: &servicehost.GRPC{
			Addr: config.GRPCAddr,
			Register: func(server *grpc.Server) {
				gateway.RegisterPushService(server, transport)
				gateway.RegisterDebugService(server, transport)
			},
		},
		GRPCPool: grpcPool,
		BeforeShutdown: func(shutdownCtx context.Context) error {
			if advertiseTarget == "" {
				return nil
			}
			return gatewayDirectory.Unregister(shutdownCtx, instanceID, advertiseTarget)
		},
		GRPCReady: func(ctx context.Context) error {
			targets := make([]string, 0, len(farmTargets)+1)
			for _, target := range farmTargets {
				targets = append(targets, target)
			}
			targets = append(targets, socialTarget)
			return grpcPool.Ready(ctx, targets...)
		},
	}).Run(ctx)
}

func gatewayAdvertiseTarget(grpcAddr string, getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", nil
	}
	if explicit := strings.TrimSpace(getenv("FARM_GATEWAY_ADVERTISE_GRPC")); explicit != "" {
		if !validAdvertiseTarget(explicit) {
			return "", fmt.Errorf("gateway: FARM_GATEWAY_ADVERTISE_GRPC must be host:port")
		}
		return explicit, nil
	}
	host := strings.TrimSpace(getenv("FARM_GATEWAY_ADVERTISE_HOST"))
	if host == "" {
		return "", nil
	}
	port := strings.TrimSpace(getenv("FARM_GATEWAY_ADVERTISE_PORT"))
	if port == "" {
		_, listenerPort, err := net.SplitHostPort(grpcAddr)
		if err != nil || listenerPort == "" {
			return "", fmt.Errorf("gateway: cannot derive advertise port from %q", grpcAddr)
		}
		port = listenerPort
	}
	target := net.JoinHostPort(host, port)
	if !validAdvertiseTarget(target) {
		return "", fmt.Errorf("gateway: dynamic advertise target must be host:port")
	}
	return target, nil
}

func validAdvertiseTarget(target string) bool {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(target))
	if err != nil || strings.TrimSpace(host) == "" {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port > 0 && port <= 65535
}

func positiveDurationSetting(name string, fallback time.Duration) (time.Duration, error) {
	raw := servicehost.Getenv(name, fallback.String())
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("gateway: %s must be a positive duration, got %q", name, raw)
	}
	return value, nil
}

func renewGatewayRegistration(
	ctx context.Context,
	directory *presence.GatewayDirectory,
	instanceID, target string,
	ttl time.Duration,
) {
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := directory.Register(ctx, instanceID, target, ttl); err != nil && ctx.Err() == nil {
				telemetry.L().Error("gateway instance lease renewal failed",
					"component", "gateway",
					"gateway_id", instanceID,
					"target", target,
					"err", err.Error(),
				)
			}
		}
	}
}

func apiDocsEnabled(environment string, getenv func(string) string) bool {
	return environment == "dev" && strings.TrimSpace(getenv("FARM_ENABLE_API_DOCS")) == "1"
}
