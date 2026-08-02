// social 是好友关系与好友申请的唯一部署入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"farm/server/api/rpc"
	"farm/server/api/socialapi"
	"farm/server/platform/obs"
	"farm/server/platform/servicehost"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	storage, closeStorage, err := servicehost.OpenStorage(ctx, config)
	if err != nil {
		return err
	}
	defer closeStorage()

	mux := http.NewServeMux()
	mux.Handle(rpc.Path, rpc.NewHandler(config.InternalToken, socialapi.NewDispatcher(storage)))
	return (servicehost.Host{
		Config:  config,
		Handler: mux,
		Checker: obs.FuncChecker("storage", storage.Ping),
	}).Run(ctx)
}
