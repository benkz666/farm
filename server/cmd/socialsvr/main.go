// social 是好友关系与好友申请的唯一部署入口。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"farm/server/farmsvr/farmrpc"
	"farm/server/shared/grpcx"
	"farm/server/shared/servicehost"
	"farm/server/shared/telemetry"
	socialapi "farm/server/socialsvr/api"

	"google.golang.org/grpc"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := servicehost.LoadConfig("social", ":9004", "127.0.0.1:9304")
	if err != nil {
		return err
	}
	if config.GRPCAddr == "" {
		config.GRPCAddr = servicehost.Getenv("FARM_SOCIAL_GRPC_ADDR", ":9204")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	storage, closeStorage, err := servicehost.OpenStorage(ctx, config)
	if err != nil {
		return err
	}
	defer closeStorage()
	friendStore := storage.CachedFriendStore()
	gatewayTargets, err := servicehost.ParseGRPCTargetMap(
		"FARM_GATEWAY_GRPC_TARGETS",
		servicehost.Getenv("FARM_GATEWAY_GRPC_TARGETS", `{"gateway-0":"127.0.0.1:9202"}`),
	)
	if err != nil {
		return err
	}
	inviteSecret := servicehost.Getenv("FARM_INVITE_SECRET", "dev-only-invite-change-me")
	if config.Environment != "dev" && len(inviteSecret) < 32 {
		return fmt.Errorf("social: FARM_INVITE_SECRET must contain at least 32 bytes outside dev")
	}
	grpcPool := grpcx.NewPool(config.InternalToken)
	pushClient := farmrpc.NewResolvingGatewayPushClient(
		grpcPool, gatewayTargets, storage.GatewayDirectory(),
	)
	mailNotifier := farmrpc.NewMailFanoutPublisher(
		storage.ConnectionRegistry(), farmrpc.NewGRPCMailNotifyPusher(pushClient),
	)
	accessRevoker := farmrpc.NewFarmAccessRevoker(
		storage.ConnectionRegistry(), farmrpc.NewGRPCFarmAccessPusher(pushClient),
	)

	return (servicehost.Host{
		Config:  config,
		Handler: http.NewServeMux(),
		Checker: telemetry.FuncChecker("storage", storage.Ping),
		GRPC: &servicehost.GRPC{
			Addr: config.GRPCAddr,
			Register: func(server *grpc.Server) {
				adapter := socialapi.RegisterGRPC(server, friendStore,
					socialapi.WithStealHints(storage),
					socialapi.WithInviteSecret([]byte(inviteSecret)),
					socialapi.WithSocialNotifier(mailNotifier),
					socialapi.WithFarmAccessRevoker(accessRevoker),
				)
				adapter.StartDistributedInvalidations(ctx)
			},
		},
		GRPCPool: grpcPool,
	}).Run(ctx)
}
