// gateway 是客户端唯一入口，负责 HTTP/WS 连接、鉴权结果转发、限流与分片路由。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"farm/server/api/authapi"
	"farm/server/api/socialapi"
	"farm/server/api/workerapi"
	"farm/server/platform/farmrpc"
	"farm/server/platform/gameconf"
	"farm/server/platform/obs"
	"farm/server/platform/servicehost"
	"farm/server/services/gateway/gateway"
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
	instanceID := servicehost.Getenv("FARM_GATEWAY_INSTANCE_ID", "gateway-0")
	routePath := servicehost.Getenv("FARM_ROUTE_TABLE", "deploy/route-table.local.json")
	routes, err := servicehost.LoadRouteTable(routePath)
	if err != nil {
		return err
	}
	farmURLs, err := servicehost.ParseEndpointMap(
		"FARM_FARM_URLS",
		servicehost.Getenv("FARM_FARM_URLS", `{"farm-0":"http://127.0.0.1:9100"}`),
	)
	if err != nil {
		return err
	}
	gatewayURLs, err := servicehost.ParseEndpointMap(
		"FARM_GATEWAY_URLS",
		servicehost.Getenv("FARM_GATEWAY_URLS", `{"gateway-0":"http://127.0.0.1:9002"}`),
	)
	if err != nil {
		return err
	}
	if gatewayURLs[instanceID] == "" {
		return fmt.Errorf("gateway: FARM_GATEWAY_URLS has no entry for %q", instanceID)
	}
	authURL, err := servicehost.RequiredURL("FARM_AUTH_URL", "http://127.0.0.1:9003")
	if err != nil {
		return err
	}
	socialURL, err := servicehost.RequiredURL("FARM_SOCIAL_URL", "http://127.0.0.1:9004")
	if err != nil {
		return err
	}
	workerURL, err := servicehost.RequiredURL("FARM_WORKER_URL", "http://127.0.0.1:9005")
	if err != nil {
		return err
	}
	timeProfile := strings.ToLower(servicehost.Getenv("FARM_TIME_PROFILE", gameconf.TimeProfileDemo))
	if !gameconf.ValidTimeProfile(timeProfile) {
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
	eventBus, err := servicehost.OpenEventBus(config, instanceID)
	if err != nil {
		return err
	}
	defer eventBus.Close()

	metrics := obs.NewMetrics(nil)
	timeProfiles := gameconf.NewTimeProfileSwitch(timeProfile)
	authClient := authapi.NewClient(authURL, config.InternalToken)
	socialClient := socialapi.NewClient(socialURL, config.InternalToken)
	workerClient := workerapi.NewClient(workerURL, config.InternalToken)
	transport := gateway.New(
		authClient,
		storage,
		nil,
		gateway.WithFarmRPC(farmrpc.NewHTTPClient(farmURLs, config.InternalToken), routes),
		gateway.WithFriendStore(socialClient),
		gateway.WithStealHintStore(storage),
		gateway.WithTaskMailStore(workerClient),
		gateway.WithCodexRewardStore(workerClient),
		gateway.WithInviteSecret([]byte(inviteSecret)),
		gateway.WithConnectionRegistry(storage.ConnectionRegistry(), instanceID),
		gateway.WithInternalPushToken(config.InternalToken),
		gateway.WithSessionKickPusher(farmrpc.NewHTTPSessionKickPusher(gatewayURLs, config.InternalToken)),
		gateway.WithTaskNotifyFanout(farmrpc.NewTaskFanoutPublisher(
			storage.ConnectionRegistry(),
			farmrpc.NewHTTPTaskNotifyPusher(gatewayURLs, config.InternalToken),
		)),
		gateway.WithDebugTimeFanout(farmURLs, gatewayURLs, config.InternalToken),
		gateway.WithCrossEventBus(eventBus),
		gateway.WithMetrics(metrics),
		gateway.WithTimeProfileSwitch(timeProfiles),
	)
	if servicehost.Getenv("FARM_ALLOW_DEBUG_TIME", "0") == "1" {
		transport.EnableDebugTime()
	}

	return (servicehost.Host{
		Config:  config,
		Handler: transport.Handler(),
		Checker: obs.FuncChecker("storage", storage.Ping),
		Metrics: metrics,
	}).Run(ctx)
}
