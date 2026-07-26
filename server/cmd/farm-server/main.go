package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/auth"
	"farm/server/internal/gateway"
	"farm/server/internal/store"
)

type config struct {
	httpAddr    string
	mysqlDSN    string
	redisAddr   string
	tokenSecret string
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	storage, closeStore, err := store.Open(startupCtx, config.mysqlDSN, config.redisAddr, 0)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer func() {
		if closeErr := closeStore(); closeErr != nil {
			log.Printf("close storage: %v", closeErr)
		}
	}()

	runtime := actor.NewRuntime(storage, 0)
	transport := gateway.New(auth.New(storage, storage), storage, runtime)
	if os.Getenv("FARM_ALLOW_DEBUG_TIME") == "1" {
		transport.EnableDebugTime()
		log.Printf("debug time advance enabled (FARM_ALLOW_DEBUG_TIME=1)")
	}
	server := &http.Server{
		Addr:              config.httpAddr,
		Handler:           transport.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()
	log.Printf("farm-server listening on %s", config.httpAddr)

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP: %w", err)
		}
		if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP after shutdown: %w", err)
		}
		return nil
	}
}

func loadConfig() config {
	return config{
		httpAddr:    getenv("FARM_HTTP_ADDR", ":9002"),
		mysqlDSN:    getenv("FARM_MYSQL_DSN", "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"),
		redisAddr:   getenv("FARM_REDIS_ADDR", "127.0.0.1:6379"),
		tokenSecret: getenv("FARM_TOKEN_SECRET", "dev-only-change-me"),
	}
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
