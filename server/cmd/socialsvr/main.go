// social 是好友关系与好友申请的唯一部署入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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

	return (servicehost.Host{
		Config:  config,
		Handler: http.NewServeMux(),
		Checker: telemetry.FuncChecker("storage", storage.Ping),
		GRPC: &servicehost.GRPC{
			Addr: config.GRPCAddr,
			Register: func(server *grpc.Server) {
				socialapi.RegisterGRPC(server, storage)
			},
		},
	}).Run(ctx)
}
