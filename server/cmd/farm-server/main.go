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
	"farm/server/internal/bus"
	"farm/server/internal/cross"
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
	gatewayURLs   map[string]string
	busKind       string
	kafkaBrokers  []string
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
	eventBus, err := newCrossBus(config)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := eventBus.Close(); closeErr != nil {
			log.Printf("close event bus: %v", closeErr)
		}
	}()

	var handler http.Handler
	switch config.role {
	case roleAll:
		runtime := actor.NewRuntime(storage, 0)
		transport := newGateway(config, storage, runtime, gateway.WithCrossEventBus(eventBus))
		owner := cross.NewOwner(runtime, storage, eventBus, transport.Now, transport, nil)
		owner.SetStealHintWriter(storage)
		owner.SetPlayerDeltaPublisher(transport)
		if err := owner.Start(ctx); err != nil {
			return fmt.Errorf("start cross owner: %w", err)
		}
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
			gateway.WithCrossEventBus(eventBus),
		)
		handler = transport.Handler()
	case roleFarm:
		routes, err := loadRouteTable(config.routeTable)
		if err != nil {
			return err
		}
		runtime := actor.NewRuntime(storage, 0)
		owns := func(uid uint64) bool {
			farmID, err := routes.FarmID(uid)
			return err == nil && farmID == config.instanceID
		}
		deltaPublisher := farmrpc.NewFanoutPublisher(
			storage.ConnectionRegistry(),
			farmrpc.NewHTTPDeltaPusher(config.gatewayURLs, config.internalToken),
		)
		playerDeltaPublisher := farmrpc.NewPlayerFanoutPublisher(
			storage.ConnectionRegistry(),
			farmrpc.NewHTTPPlayerDeltaPusher(config.gatewayURLs, config.internalToken),
		)
		owner := cross.NewOwner(runtime, storage, eventBus, nil, deltaPublisher, owns)
		owner.SetStealHintWriter(storage)
		owner.SetPlayerDeltaPublisher(playerDeltaPublisher)
		if err := owner.Start(ctx); err != nil {
			return fmt.Errorf("start cross owner: %w", err)
		}
		handler = farmrpc.NewHandler(runtime, []byte(config.internalToken), func(uid uint64) bool {
			return owns(uid)
		}, nil, farmrpc.WithDeltaPublisher(deltaPublisher), farmrpc.WithStealHintWriter(storage))
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
		gateway.WithStealHintStore(storage),
		gateway.WithTaskMailStore(storage),
		gateway.WithInviteSecret([]byte(config.inviteSecret)),
		gateway.WithConnectionRegistry(storage.ConnectionRegistry(), config.instanceID),
		gateway.WithInternalPushToken(config.internalToken),
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
	role := strings.ToLower(getenv("FARM_ROLE", roleAll))
	farmURLs, err := parseFarmURLs(os.Getenv("FARM_FARM_URLS"))
	if err != nil {
		return config{}, err
	}
	gatewayURLs, err := parseFarmURLs(os.Getenv("FARM_GATEWAY_URLS"))
	if err != nil {
		return config{}, fmt.Errorf("parse FARM_GATEWAY_URLS: %w", err)
	}
	defaultInstanceID := "farm-0"
	if role == roleGateway {
		defaultInstanceID = "gateway-0"
	}
	cfg := config{
		httpAddr:      getenv("FARM_HTTP_ADDR", ":9002"),
		mysqlDSN:      getenv("FARM_MYSQL_DSN", "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"),
		redisAddr:     getenv("FARM_REDIS_ADDR", "127.0.0.1:6379"),
		tokenSecret:   tokenSecret,
		inviteSecret:  getenv("FARM_INVITE_SECRET", tokenSecret),
		role:          role,
		instanceID:    getenv("FARM_INSTANCE_ID", defaultInstanceID),
		routeTable:    getenv("FARM_ROUTE_TABLE", "deploy/route-table.example.json"),
		internalToken: strings.TrimSpace(os.Getenv("FARM_INTERNAL_TOKEN")),
		farmURLs:      farmURLs,
		gatewayURLs:   gatewayURLs,
		busKind:       strings.ToLower(getenv("FARM_BUS", "kafka")),
		kafkaBrokers:  splitCSV(getenv("FARM_KAFKA_BROKERS", "127.0.0.1:9094")),
	}
	switch cfg.role {
	case roleAll:
		return cfg, nil
	case roleGateway:
		if cfg.internalToken == "" || len(cfg.farmURLs) == 0 {
			return config{}, errors.New("FARM_ROLE=gateway requires FARM_INTERNAL_TOKEN and FARM_FARM_URLS")
		}
	case roleFarm:
		if cfg.internalToken == "" || cfg.instanceID == "" || len(cfg.gatewayURLs) == 0 {
			return config{}, errors.New("FARM_ROLE=farm requires FARM_INTERNAL_TOKEN, FARM_INSTANCE_ID, and FARM_GATEWAY_URLS")
		}
	default:
		return config{}, fmt.Errorf("unsupported FARM_ROLE %q (want all, gateway, or farm)", cfg.role)
	}
	return cfg, nil
}

func newCrossBus(config config) (bus.EventBus, error) {
	if config.role == roleAll || config.busKind == "memory" {
		return bus.NewMemoryBus(), nil
	}
	if config.busKind != "kafka" {
		return nil, fmt.Errorf("unsupported FARM_BUS %q (want kafka or memory)", config.busKind)
	}
	eventBus, err := bus.NewKafkaBus(bus.KafkaConfig{
		Brokers: config.kafkaBrokers,
		GroupID: "farm-cross-" + config.role + "-" + config.instanceID,
	})
	if err != nil {
		return nil, fmt.Errorf("create Kafka event bus: %w", err)
	}
	return eventBus, nil
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

func splitCSV(value string) []string {
	var values []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
