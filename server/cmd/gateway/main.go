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
	"farm/server/farmsvr/crossfarm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/gateway"
	"farm/server/gateway/apidocs"
	"farm/server/gateway/presence"
	"farm/server/shared/friendauth"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/servicehost"
	"farm/server/shared/telemetry"
	socialapi "farm/server/socialsvr/api"

	farmv1 "farm/server/gen/farm/v1"

	"github.com/redis/go-redis/v9"
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
	socialClient := socialapi.NewGRPCClient(grpcPool, socialTarget)
	friends := friendauth.NewCache(socialClient)
	go startFriendInvalidationWatch(ctx, friends, socialClient)
	crossClient := crossfarm.NewGRPCClient(grpcPool, farmTargets, routes)
	crossInFlight, err := nonNegativeIntSetting("FARM_CROSS_MAX_IN_FLIGHT", 1024)
	if err != nil {
		return err
	}
	writeInFlight, err := nonNegativeIntSetting("FARM_WRITE_MAX_IN_FLIGHT", 512)
	if err != nil {
		return err
	}
	writeAdmission, closeAdmissionRedis, err := openDynamicWriteAdmission(
		ctx, config.RedisAddr, farmTargets, writeInFlight,
	)
	if err != nil {
		return err
	}
	defer closeAdmissionRedis()
	pushClient := farmrpc.NewResolvingGatewayPushClient(grpcPool, gatewayTargets, gatewayDirectory)
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
		gateway.WithDebugTimeFanout(grpcPool, farmTargets, gatewayTargets, instanceID, gatewayDirectory),
		gateway.WithCrossFarmClient(crossClient),
		gateway.WithCrossInFlightLimit(crossInFlight),
		gateway.WithWriteInFlightLimit(writeInFlight),
		gateway.WithMetrics(metrics),
		gateway.WithTimeProfileSwitch(timeProfiles),
	}
	if writeAdmission != nil {
		options = append(options, gateway.WithDynamicWriteAdmission(writeAdmission))
	}
	if servicehost.Getenv("FARM_DISABLE_WS_RATE_LIMIT", "0") == "1" {
		options = append(options, gateway.WithWSRateLimitDisabled())
	}
	if apiDocsEnabled(config.Environment, os.Getenv) {
		options = append(options, gateway.WithAPIDocs(apidocs.Handler()))
	}
	transport := gateway.New(authService, storage, nil, options...)
	if writeAdmission != nil {
		writeAdmission.Start(ctx)
	}
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

func openDynamicWriteAdmission(
	ctx context.Context,
	defaultRedisAddr string,
	farmTargets map[string]string,
	maxLimit int,
) (*gateway.DynamicWriteAdmission, func() error, error) {
	enabled, err := zeroOneSetting("FARM_WRITE_DYNAMIC_ADMISSION", true)
	if err != nil {
		return nil, nil, err
	}
	if !enabled || maxLimit <= 0 {
		return nil, func() error { return nil }, nil
	}
	config := gateway.DefaultDynamicWriteAdmissionConfig(maxLimit)
	config.MinLimit, err = positiveIntSetting("FARM_WRITE_MIN_IN_FLIGHT", config.MinLimit)
	if err != nil {
		return nil, nil, err
	}
	low, err := nonNegativeIntSetting("FARM_WRITE_BACKLOG_LOW", int(config.LowWatermark))
	if err != nil {
		return nil, nil, err
	}
	high, err := nonNegativeIntSetting("FARM_WRITE_BACKLOG_HIGH", int(config.HighWatermark))
	if err != nil {
		return nil, nil, err
	}
	hard, err := nonNegativeIntSetting("FARM_WRITE_BACKLOG_HARD", int(config.HardWatermark))
	if err != nil {
		return nil, nil, err
	}
	config.LowWatermark, config.HighWatermark, config.HardWatermark = int64(low), int64(high), int64(hard)
	config.RecoveryStep, err = positiveIntSetting("FARM_WRITE_RECOVERY_STEP", config.RecoveryStep)
	if err != nil {
		return nil, nil, err
	}
	config.PollInterval, err = positiveDurationSetting("FARM_WRITE_BACKLOG_POLL", config.PollInterval)
	if err != nil {
		return nil, nil, err
	}
	config.SampleTimeout, err = positiveDurationSetting("FARM_WRITE_BACKLOG_TIMEOUT", config.SampleTimeout)
	if err != nil {
		return nil, nil, err
	}
	config.ErrorGrace, err = nonNegativeDurationSetting("FARM_WRITE_BACKLOG_ERROR_GRACE", config.ErrorGrace)
	if err != nil {
		return nil, nil, err
	}

	shards, err := positiveIntSetting("FARM_WRITE_JOURNAL_SHARDS", 32)
	if err != nil {
		return nil, nil, err
	}
	farmIDs := make([]string, 0, len(farmTargets))
	for farmID := range farmTargets {
		farmIDs = append(farmIDs, farmID)
	}
	eventRedisAddr := servicehost.Getenv("FARM_EVENT_REDIS_ADDR", defaultRedisAddr)
	client := redis.NewClient(&redis.Options{
		Addr:         eventRedisAddr,
		PoolSize:     4,
		MinIdleConns: 1,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  config.SampleTimeout,
		WriteTimeout: config.SampleTimeout,
	})
	startupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err = client.Ping(startupCtx).Err()
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("gateway: connect write-journal Redis: %w", err)
	}
	source, err := gateway.NewRedisWriteBacklogSource(
		client,
		servicehost.Getenv("FARM_WRITE_JOURNAL_PREFIX", "farm:write"),
		farmIDs,
		shards,
	)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("gateway: configure write backlog source: %w", err)
	}
	admission, err := gateway.NewDynamicWriteAdmission(source, config)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("gateway: configure dynamic write admission: %w", err)
	}
	return admission, client.Close, nil
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

func nonNegativeDurationSetting(name string, fallback time.Duration) (time.Duration, error) {
	raw := servicehost.Getenv(name, fallback.String())
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("gateway: %s must be a non-negative duration, got %q", name, raw)
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

func nonNegativeIntSetting(name string, fallback int) (int, error) {
	raw := servicehost.Getenv(name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("gateway: %s must be a non-negative integer, got %q", name, raw)
	}
	return value, nil
}

func positiveIntSetting(name string, fallback int) (int, error) {
	value, err := nonNegativeIntSetting(name, fallback)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, fmt.Errorf("gateway: %s must be a positive integer", name)
	}
	return value, nil
}

func zeroOneSetting(name string, fallback bool) (bool, error) {
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
		return false, fmt.Errorf("gateway: %s must be 0 or 1, got %q", name, raw)
	}
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
