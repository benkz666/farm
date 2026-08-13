// farm 是用户 Actor 与农场权威状态的唯一部署入口。
package main

import (
	"context"
	"errors"
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
	"farm/server/shared/store"
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
	stealHints := store.NewAsyncStealHintStore(storage)
	defer stealHints.Shutdown(context.Background())
	metrics := telemetry.NewMetrics(nil)
	journalConfig := store.DefaultFarmWriteJournalConfig(instanceID)
	journalConfig.Prefix = servicehost.Getenv("FARM_WRITE_JOURNAL_PREFIX", journalConfig.Prefix)
	journalConfig.Shards, err = intSetting("FARM_WRITE_JOURNAL_SHARDS", journalConfig.Shards)
	if err != nil || journalConfig.Shards == 0 {
		return fmt.Errorf("farm: FARM_WRITE_JOURNAL_SHARDS must be positive")
	}
	journalConfig.Projectors, err = intSetting("FARM_WRITE_JOURNAL_PROJECTORS", journalConfig.Projectors)
	if err != nil || journalConfig.Projectors == 0 {
		return fmt.Errorf("farm: FARM_WRITE_JOURNAL_PROJECTORS must be positive")
	}
	journalConfig.BatchSize = int64Value("FARM_WRITE_JOURNAL_BATCH", journalConfig.BatchSize)
	journalConfig.Block, err = durationSetting("FARM_WRITE_JOURNAL_BLOCK", journalConfig.Block.String())
	if err != nil {
		return err
	}
	journalConfig.LatestTTL, err = durationSetting("FARM_WRITE_JOURNAL_LATEST_TTL", journalConfig.LatestTTL.String())
	if err != nil {
		return err
	}
	journalConfig.ClaimIdle, err = durationSetting("FARM_WRITE_JOURNAL_CLAIM_IDLE", journalConfig.ClaimIdle.String())
	if err != nil {
		return err
	}
	journalConfig.ReplicaAcks, err = intSetting("FARM_WRITE_JOURNAL_REPLICAS", 0)
	if err != nil {
		return err
	}
	journal, closeJournalRedis, err := store.OpenFarmWriteJournal(
		ctx,
		storage,
		config.RedisAddr,
		journalConfig,
	)
	if err != nil {
		return err
	}
	defer closeJournalRedis()
	journal.SetMetrics(metrics)
	if err := journal.Start(ctx); err != nil {
		return err
	}
	recoveryTimeout, err := durationSetting("FARM_WRITE_JOURNAL_RECOVERY_TIMEOUT", "5m")
	if err != nil || recoveryTimeout <= 0 {
		return fmt.Errorf("farm: FARM_WRITE_JOURNAL_RECOVERY_TIMEOUT must be positive")
	}
	recoveryContext, cancelRecovery := context.WithTimeout(ctx, recoveryTimeout)
	if err := journal.WaitIdle(recoveryContext); err != nil {
		cancelRecovery()
		return fmt.Errorf("farm: recover write journal before serving: %w", err)
	}
	cancelRecovery()

	var writeAdmission *farmrpc.DynamicWriteAdmission
	writeAdmissionEnabled, err := boolSetting("FARM_WRITE_DYNAMIC_ADMISSION", true)
	if err != nil {
		return err
	}
	if writeAdmissionEnabled {
		writeMax, settingErr := intSetting("FARM_WRITE_MAX_IN_FLIGHT", 512)
		if settingErr != nil || writeMax <= 0 {
			return fmt.Errorf("farm: FARM_WRITE_MAX_IN_FLIGHT must be positive")
		}
		admissionConfig := farmrpc.DefaultWriteAdmissionConfig(writeMax)
		admissionConfig.MinLimit, err = intSetting("FARM_WRITE_MIN_IN_FLIGHT", admissionConfig.MinLimit)
		if err != nil || admissionConfig.MinLimit <= 0 {
			return fmt.Errorf("farm: FARM_WRITE_MIN_IN_FLIGHT must be positive")
		}
		admissionConfig.LowWatermark = int64Value("FARM_WRITE_BACKLOG_LOW", admissionConfig.LowWatermark)
		admissionConfig.HighWatermark = int64Value("FARM_WRITE_BACKLOG_HIGH", admissionConfig.HighWatermark)
		admissionConfig.HardWatermark = int64Value("FARM_WRITE_BACKLOG_HARD", admissionConfig.HardWatermark)
		admissionConfig.RecoveryStep, err = intSetting("FARM_WRITE_RECOVERY_STEP", admissionConfig.RecoveryStep)
		if err != nil || admissionConfig.RecoveryStep <= 0 {
			return fmt.Errorf("farm: FARM_WRITE_RECOVERY_STEP must be positive")
		}
		admissionConfig.PollInterval, err = durationSetting("FARM_WRITE_BACKLOG_POLL", admissionConfig.PollInterval.String())
		if err != nil {
			return err
		}
		admissionConfig.SampleTimeout, err = durationSetting("FARM_WRITE_BACKLOG_TIMEOUT", admissionConfig.SampleTimeout.String())
		if err != nil {
			return err
		}
		admissionConfig.ErrorGrace, err = nonNegativeDurationSetting("FARM_WRITE_BACKLOG_ERROR_GRACE", admissionConfig.ErrorGrace.String())
		if err != nil {
			return err
		}
		admissionConfig.AdmissionWait, err = nonNegativeDurationSetting("FARM_WRITE_ADMISSION_WAIT", admissionConfig.AdmissionWait.String())
		if err != nil {
			return err
		}
		writeAdmission, err = farmrpc.NewDynamicWriteAdmission(journal, admissionConfig)
		if err != nil {
			return err
		}
		writeAdmission.SetMetrics(metrics)
		writeAdmission.Start(ctx)
	}

	actorIdleTTL, err := durationSetting("FARM_ACTOR_IDLE_TTL", "2m")
	if err != nil {
		return err
	}
	actorMaxResident, err := intSetting("FARM_ACTOR_MAX_RESIDENT", 20_000)
	if err != nil {
		return err
	}
	committerShards, err := intSetting("FARM_COMMITTER_SHARDS", 16)
	if err != nil || committerShards == 0 {
		return fmt.Errorf("farm: FARM_COMMITTER_SHARDS must be positive")
	}
	runtime := room.NewRuntime(journal.WrapFarmStore(storage), actorIdleTTL)
	runtime.SetCommitterShards(committerShards)
	runtime.SetMaxResident(actorMaxResident)
	runtime.SetHazardSalt(farm.DeriveHazardSalt(hazardSecret))
	runtime.SetMetrics(metrics)
	owns := func(uid uint64) bool {
		farmID, routeErr := routes.FarmID(uid)
		return routeErr == nil && farmID == instanceID
	}

	grpcPool := grpcx.NewPool(config.InternalToken)
	pushClient := farmrpc.NewResolvingGatewayPushClient(
		grpcPool,
		gatewayTargets,
		storage.GatewayDirectory(),
	)
	fanoutPublisher := farmrpc.NewFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewGRPCDeltaPusher(pushClient),
	)
	fanoutPublisher.SetMetrics(metrics)
	deltaPublisher := farmrpc.NewAsyncDeltaPublisher(fanoutPublisher, 16, 1024)
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
	journal.SetTaskObserver(func(uid uint64, task store.Task) {
		if err := taskNotifyPublisher.PublishTaskNotify(context.Background(), uid, task); err != nil {
			telemetry.L().Error("write journal task notify failed",
				"component", "write_journal", "uid", uid, "err", err.Error())
		}
	})
	journal.SetMailObserver(func(uid uint64) {
		if err := mailNotifyPublisher.PublishMailNotify(context.Background(), uid, "codex_reward"); err != nil {
			telemetry.L().Error("write journal mail notify failed",
				"component", "write_journal", "uid", uid, "err", err.Error())
		}
	})
	directStore := journal.WrapDirectStore(storage)
	socialClient := socialapi.NewGRPCClient(grpcPool, socialTarget)
	friends := friendauth.NewCache(socialClient)
	go startFriendInvalidationWatch(ctx, friends, socialClient)
	clock := &testclock.Clock{}
	owner := crossfarm.NewOwner(runtime, friends, clock.Now, deltaPublisher, owns)
	owner.SetStealHintWriter(stealHints)
	owner.SetPlayerDeltaPublisher(playerDeltaPublisher)
	visitor := crossfarm.NewVisitorSettler(runtime, clock.Now)
	crossClient := crossfarm.NewGRPCClient(grpcPool, farmTargets, routes)
	// Outbox retry/lease 使用真实墙钟，不能跟随玩法调时；否则 debug advance 会让
	// 刚写入的 fallback 立即到期并与健康的直连结算重复竞争。
	dispatcher := crossfarm.NewOutboxDispatcher(storage, crossClient, nil, playerDeltaPublisher)

	timeProfiles := gameconfig.NewTimeProfileSwitch(timeProfile)
	commandHandler := farmrpc.NewHandler(
		runtime,
		[]byte(config.InternalToken),
		owns,
		clock.Now,
		farmrpc.WithDeltaPublisher(deltaPublisher),
		farmrpc.WithPlayerDeltaPublisher(playerDeltaPublisher),
		farmrpc.WithTaskNotifyPublisher(taskNotifyPublisher),
		farmrpc.WithStealHintWriter(stealHints),
		farmrpc.WithTaskMailStore(storage),
		farmrpc.WithTaskProgressWriter(journal),
		farmrpc.WithTaskClaimer(directStore),
		farmrpc.WithDailyLoginClaimer(directStore),
		farmrpc.WithMailClaimer(directStore),
		farmrpc.WithCodexRewardStore(journal),
		farmrpc.WithBundledJournalSideEffects(),
		farmrpc.WithMailNotifyPublisher(mailNotifyPublisher),
		farmrpc.WithTimeProfileSwitch(timeProfiles),
	)
	owner.SetAdvanceScheduler(commandHandler.ScheduleAdvanceAt)
	crossServer := crossfarm.NewGRPCServer(
		owner, visitor, owns, journal.WrapOutboxStore(storage),
	)
	crossCoordinator := crossfarm.NewClientCoordinator(
		friends, crossServer, crossClient, clock.Now, timeProfiles.Get,
	)
	clientHandler := farmrpc.NewClientHandler(commandHandler, friends, crossCoordinator, owns, writeAdmission)

	return (servicehost.Host{
		Config:  config,
		Handler: http.NewServeMux(),
		Checker: telemetry.FuncChecker("storage_and_write_journal", func(ctx context.Context) error {
			if err := storage.Ping(ctx); err != nil {
				return err
			}
			return journal.Ping(ctx)
		}),
		Metrics: metrics,
		GRPC: &servicehost.GRPC{
			Addr: config.GRPCAddr,
			Register: func(server *grpc.Server) {
				farmrpc.RegisterCommandService(server, clientHandler, owns)
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
			return errors.Join(
				runtime.Shutdown(ctx),
				deltaPublisher.Shutdown(ctx),
				stealHints.Shutdown(ctx),
				journal.Shutdown(ctx),
			)
		},
	}).Run(ctx)
}

func int64Value(name string, fallback int64) int64 {
	raw := servicehost.Getenv(name, strconv.FormatInt(fallback, 10))
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func durationSetting(name, fallback string) (time.Duration, error) {
	raw := servicehost.Getenv(name, fallback)
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("farm: %s must be a positive duration, got %q", name, raw)
	}
	return value, nil
}

func nonNegativeDurationSetting(name, fallback string) (time.Duration, error) {
	raw := servicehost.Getenv(name, fallback)
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("farm: %s must be a non-negative duration, got %q", name, raw)
	}
	return value, nil
}

func boolSetting(name string, fallback bool) (bool, error) {
	fallbackText := "0"
	if fallback {
		fallbackText = "1"
	}
	raw := strings.TrimSpace(servicehost.Getenv(name, fallbackText))
	switch raw {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("farm: %s must be 0 or 1, got %q", name, raw)
	}
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
