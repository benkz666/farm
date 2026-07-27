package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/auth"
	"farm/server/internal/farmrpc"
	"farm/server/internal/gateway"
	"farm/server/internal/routing"
	"farm/server/internal/store"
)

const (
	roleAll     = "all"
	roleGateway = "gateway"
	roleFarm    = "farm"
)

type config struct {
	httpAddr      string
	mysqlDSN      string
	redisAddr     string
	tokenSecret   string
	inviteSecret  string
	role          string
	instanceID    string
	routeTable    string
	internalToken string
	farmURLs      map[string]string
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
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

	var handler http.Handler
	switch config.role {
	case roleAll:
		runtime := actor.NewRuntime(storage, 0)
		transport := newGateway(config, storage, runtime)
		if os.Getenv("FARM_ALLOW_DEBUG_TIME") == "1" {
			transport.EnableDebugTime()
			log.Printf("debug time advance enabled (FARM_ALLOW_DEBUG_TIME=1)")
		}
		handler = combineHandlers(
			transport.Handler(),
			farmrpc.NewHandler(runtime, []byte(config.internalToken), func(uint64) bool { return true }, transport.Now),
		)
	case roleGateway:
		routes, err := loadRouteTable(config.routeTable)
		if err != nil {
			return err
		}
		transport := newGateway(
			config,
			storage,
			nil,
			gateway.WithFarmRPC(farmrpc.NewHTTPClient(config.farmURLs, config.internalToken), routes),
		)
		handler = transport.Handler()
	case roleFarm:
		routes, err := loadRouteTable(config.routeTable)
		if err != nil {
			return err
		}
		runtime := actor.NewRuntime(storage, 0)
		handler = farmrpc.NewHandler(runtime, []byte(config.internalToken), func(uid uint64) bool {
			farmID, err := routes.FarmID(uid)
			return err == nil && farmID == config.instanceID
		}, nil)
	default:
		return fmt.Errorf("unsupported FARM_ROLE %q", config.role)
	}
	server := &http.Server{
		Addr:              config.httpAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()
	log.Printf("farm-server role=%s listening on %s", config.role, config.httpAddr)

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

func newGateway(config config, storage *store.Store, runtime gateway.FarmRuntime, options ...gateway.Option) *gateway.Gateway {
	baseOptions := []gateway.Option{
		gateway.WithFriendStore(storage),
		gateway.WithInviteSecret([]byte(config.inviteSecret)),
	}
	return gateway.New(auth.New(storage, storage), storage, runtime, append(baseOptions, options...)...)
}

func combineHandlers(public http.Handler, internal http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/internal/v1/cmd", internal)
	mux.Handle("/", public)
	return mux
}

func loadRouteTable(path string) (*routing.RouteTable, error) {
	routes, err := routing.LoadRouteTable(path)
	if err == nil || filepath.IsAbs(path) {
		if err != nil {
			return nil, fmt.Errorf("load route table: %w", err)
		}
		return routes, nil
	}
	routes, parentErr := routing.LoadRouteTable(filepath.Join("..", path))
	if parentErr != nil {
		return nil, fmt.Errorf("load route table %q: %w", path, err)
	}
	return routes, nil
}

func loadConfig() (config, error) {
	tokenSecret := getenv("FARM_TOKEN_SECRET", "dev-only-change-me")
	farmURLs, err := parseFarmURLs(os.Getenv("FARM_FARM_URLS"))
	if err != nil {
		return config{}, err
	}
	cfg := config{
		httpAddr:      getenv("FARM_HTTP_ADDR", ":9002"),
		mysqlDSN:      getenv("FARM_MYSQL_DSN", "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"),
		redisAddr:     getenv("FARM_REDIS_ADDR", "127.0.0.1:6379"),
		tokenSecret:   tokenSecret,
		inviteSecret:  getenv("FARM_INVITE_SECRET", tokenSecret),
		role:          strings.ToLower(getenv("FARM_ROLE", roleAll)),
		instanceID:    getenv("FARM_INSTANCE_ID", "farm-0"),
		routeTable:    getenv("FARM_ROUTE_TABLE", "deploy/route-table.example.json"),
		internalToken: strings.TrimSpace(os.Getenv("FARM_INTERNAL_TOKEN")),
		farmURLs:      farmURLs,
	}
	switch cfg.role {
	case roleAll:
		return cfg, nil
	case roleGateway:
		if cfg.internalToken == "" || len(cfg.farmURLs) == 0 {
			return config{}, errors.New("FARM_ROLE=gateway requires FARM_INTERNAL_TOKEN and FARM_FARM_URLS")
		}
	case roleFarm:
		if cfg.internalToken == "" || cfg.instanceID == "" {
			return config{}, errors.New("FARM_ROLE=farm requires FARM_INTERNAL_TOKEN and FARM_INSTANCE_ID")
		}
	default:
		return config{}, fmt.Errorf("unsupported FARM_ROLE %q (want all, gateway, or farm)", cfg.role)
	}
	return cfg, nil
}

func parseFarmURLs(value string) (map[string]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var endpoints map[string]string
	if err := json.Unmarshal([]byte(value), &endpoints); err != nil {
		return nil, fmt.Errorf("parse FARM_FARM_URLS: %w", err)
	}
	for farmID, endpoint := range endpoints {
		if strings.TrimSpace(farmID) == "" || strings.TrimSpace(endpoint) == "" {
			return nil, errors.New("FARM_FARM_URLS contains an empty farm ID or endpoint")
		}
	}
	return endpoints, nil
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
