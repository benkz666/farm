// worker 是任务、邮件与图鉴里程碑奖励的唯一部署入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"farm/server/api/rpc"
	"farm/server/api/workerapi"
	"farm/server/platform/obs"
	"farm/server/platform/servicehost"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := servicehost.LoadConfig("worker", ":9005", "127.0.0.1:9305")
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
	mux.Handle(rpc.Path, rpc.NewHandler(config.InternalToken, workerapi.NewDispatcher(storage)))
	return (servicehost.Host{
		Config:  config,
		Handler: mux,
		Checker: obs.FuncChecker("storage", storage.Ping),
	}).Run(ctx)
}
