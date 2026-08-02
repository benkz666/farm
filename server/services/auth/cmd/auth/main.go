// auth 是账号注册、密码校验与会话签发的唯一部署入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"farm/server/api/authapi"
	"farm/server/api/rpc"
	"farm/server/platform/obs"
	"farm/server/platform/servicehost"
	"farm/server/services/auth/internal/auth"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := servicehost.LoadConfig("auth", ":9003", "127.0.0.1:9303")
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
	mux.Handle(rpc.Path, rpc.NewHandler(
		config.InternalToken,
		authapi.NewDispatcher(auth.New(storage, storage)),
	))
	return (servicehost.Host{
		Config:  config,
		Handler: mux,
		Checker: obs.FuncChecker("storage", storage.Ping),
	}).Run(ctx)
}
