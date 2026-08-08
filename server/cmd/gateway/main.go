// gateway 是客户端唯一入口，负责 HTTP/WS 连接、鉴权结果转发、限流与分片路由。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"farm/server/auth"
	"farm/server/farmsvr/crossfarm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/gateway"
	"farm/server/gateway/apidocs"
	"farm/server/shared/friendauth"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/servicehost"
	"farm/server/shared/telemetry"
	socialapi "farm/server/socialsvr/api"

	farmv1 "farm/server/gen/farm/v1"

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
	if gatewayTargets[instanceID] == "" {
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
	inviteSecret := servicehost.Getenv("FARM_INVITE_SECRET", "dev-only-invite-change-me")
	if config.Environment != "dev" && len(inviteSecret) < 32 {
		return fmt.Errorf("gateway: FARM_INVITE_SECRET must contain at least 32 bytes outside dev")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	storage, closeStorage, err := servicehost.OpenStorage(ctx, config)
	if err != nil {
		return err
	}
	defer closeStorage()

	metrics := telemetry.NewMetrics(nil)
	timeProfiles := gameconfig.NewTimeProfileSwitch(timeProfile)
	authService := auth.New(storage, storage)
	grpcPool := grpcx.NewPool(config.InternalToken)
	socialClient := socialapi.NewGRPCClient(grpcPool, socialTarget)
	friends := friendauth.NewCache(socialClient)
	go startFriendInvalidationWatch(ctx, friends, socialClient)
	crossClient := crossfarm.NewGRPCClient(grpcPool, farmTargets, routes)
	crossInFlight, err := nonNegativeIntSetting("FARM_CROSS_MAX_IN_FLIGHT", 1024)
	if err != nil {
		return err
	}
	pushClient := farmrpc.NewGatewayPushClient(grpcPool, gatewayTargets)
	options := []gateway.Option{
		gateway.WithFarmRPC(farmrpc.NewGRPCClient(grpcPool, farmTargets), routes),
		gateway.WithFriendStore(friends),
		gateway.WithStealHintStore(storage),
		gateway.WithInviteSecret([]byte(inviteSecret)),
		gateway.WithConnectionRegistry(storage.ConnectionRegistry(), instanceID),
		gateway.WithSessionKickPusher(farmrpc.NewGRPCSessionKickPusher(pushClient)),
		gateway.WithTaskNotifyFanout(farmrpc.NewTaskFanoutPublisher(
			storage.ConnectionRegistry(),
			farmrpc.NewGRPCTaskNotifyPusher(pushClient),
		)),
		gateway.WithDebugTimeFanout(grpcPool, farmTargets, gatewayTargets, instanceID),
		gateway.WithCrossFarmClient(crossClient),
		gateway.WithCrossInFlightLimit(crossInFlight),
		gateway.WithMetrics(metrics),
		gateway.WithTimeProfileSwitch(timeProfiles),
	}
	if servicehost.Getenv("FARM_DISABLE_WS_RATE_LIMIT", "0") == "1" {
		options = append(options, gateway.WithWSRateLimitDisabled())
	}
	if apiDocsEnabled(config.Environment, os.Getenv) {
		options = append(options, gateway.WithAPIDocs(apidocs.Handler()))
	}
	transport := gateway.New(authService, storage, nil, options...)
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

func nonNegativeIntSetting(name string, fallback int) (int, error) {
	raw := servicehost.Getenv(name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("gateway: %s must be a non-negative integer, got %q", name, raw)
	}
	return value, nil
}

func apiDocsEnabled(environment string, getenv func(string) string) bool {
	return environment == "dev" && strings.TrimSpace(getenv("FARM_ENABLE_API_DOCS")) == "1"
}

func startFriendInvalidationWatch(ctx context.Context, cache *friendauth.Cache, social *socialapi.GRPCClient) {
	go cache.WatchInvalidations(ctx, 0, func(ctx context.Context, _ uint64) (<-chan *farmv1.FriendInvalidation, error) {
		client, err := social.SocialService(ctx)
		if err != nil {
			return nil, err
		}
		return friendauth.GRPCWatch(client)(ctx, 0)
	})
}
