// farm 是用户 Actor 与农场权威状态的唯一部署入口。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/crossfarm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/farmsvr/room"
	"farm/server/shared/friendauth"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/servicehost"
	"farm/server/shared/telemetry"
	"farm/server/shared/testclock"
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
	config, err := servicehost.LoadConfig("farm", ":9100", "127.0.0.1:9310")
	if err != nil {
		return err
	}
	if config.GRPCAddr == "" {
		config.GRPCAddr = servicehost.Getenv("FARM_FARM_GRPC_ADDR", ":9210")
	}
	instanceID := servicehost.Getenv("FARM_FARM_INSTANCE_ID", "farm-0")
	routes, err := servicehost.LoadRouteTable(
		servicehost.Getenv("FARM_ROUTE_TABLE", "deploy/route-table.local.json"),
	)
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
	socialTarget, err := servicehost.RequiredGRPCTarget(
		"FARM_SOCIAL_GRPC_TARGET",
		"127.0.0.1:9204",
	)
	if err != nil {
		return err
	}
	timeProfile := strings.ToLower(servicehost.Getenv("FARM_TIME_PROFILE", gameconfig.TimeProfileDemo))
	if !gameconfig.ValidTimeProfile(timeProfile) {
		return fmt.Errorf("farm: unsupported FARM_TIME_PROFILE %q", timeProfile)
	}
	hazardSecret := servicehost.Getenv("FARM_HAZARD_SECRET", "dev-only-hazard-secret")
	if config.Environment != "dev" && len(hazardSecret) < 32 {
		return fmt.Errorf("farm: FARM_HAZARD_SECRET must contain at least 32 bytes outside dev")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	storage, closeStorage, err := servicehost.OpenStorage(ctx, config)
	if err != nil {
		return err
	}
	defer closeStorage()

	actorIdleTTL, err := durationSetting("FARM_ACTOR_IDLE_TTL", "2m")
	if err != nil {
		return err
	}
	actorMaxResident, err := intSetting("FARM_ACTOR_MAX_RESIDENT", 20_000)
	if err != nil {
		return err
	}
	metrics := telemetry.NewMetrics(nil)
	runtime := room.NewRuntime(storage, actorIdleTTL)
	runtime.SetMaxResident(actorMaxResident)
	runtime.SetHazardSalt(farm.DeriveHazardSalt(hazardSecret))
	runtime.SetMetrics(metrics)
	owns := func(uid uint64) bool {
		farmID, routeErr := routes.FarmID(uid)
		return routeErr == nil && farmID == instanceID
	}

	grpcPool := grpcx.NewPool(config.InternalToken)
	pushClient := farmrpc.NewGatewayPushClient(grpcPool, gatewayTargets)
	deltaPublisher := farmrpc.NewFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewGRPCDeltaPusher(pushClient),
	)
	deltaPublisher.SetMetrics(metrics)
	playerDeltaPublisher := farmrpc.NewPlayerFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewGRPCPlayerDeltaPusher(pushClient),
	)
	taskNotifyPublisher := farmrpc.NewTaskFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewGRPCTaskNotifyPusher(pushClient),
	)
	mailNotifyPublisher := farmrpc.NewMailFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewGRPCMailNotifyPusher(pushClient),
	)
	socialClient := socialapi.NewGRPCClient(grpcPool, socialTarget)
	friends := friendauth.NewCache(socialClient)
	go startFriendInvalidationWatch(ctx, friends, socialClient)
	clock := &testclock.Clock{}
	owner := crossfarm.NewOwner(runtime, friends, clock.Now, deltaPublisher, owns)
	owner.SetStealHintWriter(storage)
	owner.SetPlayerDeltaPublisher(playerDeltaPublisher)
	visitor := crossfarm.NewVisitorSettler(runtime, clock.Now)
	crossClient := crossfarm.NewGRPCClient(grpcPool, farmTargets, routes)
	// Outbox retry/lease 使用真实墙钟，不能跟随玩法调时；否则 debug advance 会让
	// 刚写入的 fallback 立即到期并与健康的直连结算重复竞争。
	dispatcher := crossfarm.NewOutboxDispatcher(storage, crossClient, nil)

	timeProfiles := gameconfig.NewTimeProfileSwitch(timeProfile)
	commandHandler := farmrpc.NewHandler(
		runtime,
		[]byte(config.InternalToken),
		owns,
		clock.Now,
		farmrpc.WithDeltaPublisher(deltaPublisher),
		farmrpc.WithPlayerDeltaPublisher(playerDeltaPublisher),
		farmrpc.WithTaskNotifyPublisher(taskNotifyPublisher),
		farmrpc.WithStealHintWriter(storage),
		farmrpc.WithTaskMailStore(storage),
		farmrpc.WithTaskProgressWriter(storage),
		farmrpc.WithTaskClaimer(storage),
		farmrpc.WithDailyLoginClaimer(storage),
		farmrpc.WithMailClaimer(storage),
		farmrpc.WithCodexRewardStore(storage),
		farmrpc.WithMailNotifyPublisher(mailNotifyPublisher),
		farmrpc.WithTimeProfileSwitch(timeProfiles),
	)
	owner.SetAdvanceScheduler(commandHandler.ScheduleAdvanceAt)
	crossServer := crossfarm.NewGRPCServer(owner, visitor, owns, playerDeltaPublisher, storage)

	return (servicehost.Host{
		Config:  config,
		Handler: http.NewServeMux(),
		Checker: telemetry.FuncChecker("storage", storage.Ping),
		Metrics: metrics,
		GRPC: &servicehost.GRPC{
			Addr: config.GRPCAddr,
			Register: func(server *grpc.Server) {
				farmrpc.RegisterCommandService(server, commandHandler, owns)
				crossfarm.RegisterCrossFarmService(server, crossServer)
				if servicehost.Getenv("FARM_ALLOW_DEBUG_TIME", "0") == "1" {
					farmv1.RegisterDebugServiceServer(server, farmrpc.NewDebugServer(clock.Advance, clock.Now, timeProfiles))
				}
			},
		},
		GRPCPool: grpcPool,
		GRPCReady: func(ctx context.Context) error {
			return grpcPool.Ready(ctx, socialTarget)
		},
		BeforeShutdown: func(ctx context.Context) error {
			_ = dispatcher.Shutdown(ctx)
			commandHandler.Shutdown()
			return runtime.Shutdown(ctx)
		},
	}).Run(ctx)
}

func durationSetting(name, fallback string) (time.Duration, error) {
	raw := servicehost.Getenv(name, fallback)
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("farm: %s must be a positive duration, got %q", name, raw)
	}
	return value, nil
}

func intSetting(name string, fallback int) (int, error) {
	raw := servicehost.Getenv(name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("farm: %s must be a non-negative integer, got %q", name, raw)
	}
	return value, nil
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
