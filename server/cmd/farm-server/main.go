package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
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
	"farm/server/internal/debugclock"
	"farm/server/internal/farm"
	"farm/server/internal/farmrpc"
	"farm/server/internal/gateway"
	"farm/server/internal/obs"
	"farm/server/internal/routing"
	"farm/server/internal/store"
)

const (
	roleAll     = "all"
	roleGateway = "gateway"
	roleFarm    = "farm"
)

const (
	// envDev 是唯一允许使用占位密钥的环境名，任何其他取值都会触发密钥强度检查。
	envDev = "dev"

	// devTokenSecret 是本地开发的占位密钥，公开在源码里。
	devTokenSecret = "dev-only-change-me"

	// devHazardSecret 仅供 FARM_ENV=dev 本地联调；公开在源码里，禁止带进非 dev。
	devHazardSecret = "dev-only-hazard-secret"

	// minSecretLength 是非 dev 环境下密钥的最小长度，对应 32 字节随机串。
	minSecretLength = 32
)

const (
	// httpShutdownTimeout 用于停止接收新连接并等待在途请求收尾。
	httpShutdownTimeout = 10 * time.Second

	// actorDrainTimeout 用于把驻留 Actor 的内存权威落盘。
	// 必须在 HTTP 停止之后才开始，否则新请求会不断把 Actor 重新唤醒。
	actorDrainTimeout = 30 * time.Second
)

type config struct {
	env           string
	httpAddr      string
	adminAddr     string
	adminEnabled  bool
	mysqlDSN      string
	redisAddr     string
	tokenSecret   string
	inviteSecret  string
	hazardSecret  string
	hazardSalt    uint64
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
	obs.SetDefault(obs.NewLogger(os.Stderr, slog.LevelInfo))
	logger := obs.L()

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
			logger.Error("close storage failed", "component", "main", "op", "close_store", "err", closeErr.Error())
		}
	}()
	eventBus, err := newCrossBus(config)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := eventBus.Close(); closeErr != nil {
			logger.Error("close event bus failed", "component", "main", "op", "close_bus", "err", closeErr.Error())
		}
	}()

	metrics := obs.NewMetrics(nil)
	probe := obs.NewProbe(obs.FuncChecker("storage", storage.Ping))

	var admin *obs.Admin
	if config.adminEnabled {
		admin, err = obs.StartAdmin(obs.AdminConfig{
			Addr:     config.adminAddr,
			Probe:    probe,
			Gatherer: metrics.Registry,
		})
		if err != nil {
			return fmt.Errorf("start admin listener: %w", err)
		}
		logger.Info("admin listening",
			"component", "main",
			"op", "listen_admin",
			"addr", admin.Addr(),
		)
	}

	var handler http.Handler
	// farmRuntime 在关停时需要被疏散落盘，因此必须活到 switch 之外。
	var farmRuntime *actor.Runtime
	var deltaPublisher *farmrpc.FanoutPublisher
	switch config.role {
	case roleAll:
		runtime := actor.NewRuntime(storage, 0)
		runtime.SetHazardSalt(config.hazardSalt)
		runtime.SetMetrics(metrics)
		farmRuntime = runtime
		transport := newGateway(config, storage, runtime, metrics, gateway.WithCrossEventBus(eventBus))
		owner := cross.NewOwner(runtime, storage, eventBus, transport.Now, transport, nil)
		owner.SetStealHintWriter(storage)
		owner.SetPlayerDeltaPublisher(transport)
		if err := owner.Start(ctx); err != nil {
			return fmt.Errorf("start cross owner: %w", err)
		}
		if os.Getenv("FARM_ALLOW_DEBUG_TIME") == "1" {
			transport.EnableDebugTime()
			logger.Info("debug time advance enabled", "component", "main", "op", "enable_debug_time")
		}
		handler = combineHandlers(
			transport.Handler(),
			farmrpc.NewHandler(
				runtime,
				[]byte(config.internalToken),
				func(uint64) bool { return true },
				transport.Now,
				farmrpc.WithPlayerDeltaPublisher(transport),
				farmrpc.WithTaskProgressWriter(storage),
				farmrpc.WithMailClaimer(storage),
			),
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
			metrics,
			gateway.WithFarmRPC(farmrpc.NewHTTPClient(config.farmURLs, config.internalToken), routes),
			gateway.WithDebugTimeFanout(config.farmURLs, config.gatewayURLs, config.internalToken),
			gateway.WithCrossEventBus(eventBus),
		)
		if os.Getenv("FARM_ALLOW_DEBUG_TIME") == "1" {
			transport.EnableDebugTime()
			logger.Info("debug time advance enabled", "component", "main", "op", "enable_debug_time")
		}
		handler = transport.Handler()
	case roleFarm:
		routes, err := loadRouteTable(config.routeTable)
		if err != nil {
			return err
		}
		runtime := actor.NewRuntime(storage, 0)
		runtime.SetHazardSalt(config.hazardSalt)
		runtime.SetMetrics(metrics)
		farmRuntime = runtime
		owns := func(uid uint64) bool {
			farmID, err := routes.FarmID(uid)
			return err == nil && farmID == config.instanceID
		}
		deltaPublisher = farmrpc.NewFanoutPublisher(
			storage.ConnectionRegistry(),
			farmrpc.NewHTTPDeltaPusher(config.gatewayURLs, config.internalToken),
		)
		deltaPublisher.SetMetrics(metrics)
		playerDeltaPublisher := farmrpc.NewPlayerFanoutPublisher(
			storage.ConnectionRegistry(),
			farmrpc.NewHTTPPlayerDeltaPusher(config.gatewayURLs, config.internalToken),
		)
		clock := &debugclock.Clock{}
		owner := cross.NewOwner(runtime, storage, eventBus, clock.Now, deltaPublisher, owns)
		owner.SetStealHintWriter(storage)
		owner.SetPlayerDeltaPublisher(playerDeltaPublisher)
		if err := owner.Start(ctx); err != nil {
			return fmt.Errorf("start cross owner: %w", err)
		}
		rpcHandler := farmrpc.NewHandler(
			runtime,
			[]byte(config.internalToken),
			func(uid uint64) bool { return owns(uid) },
			clock.Now,
			farmrpc.WithDeltaPublisher(deltaPublisher),
			farmrpc.WithPlayerDeltaPublisher(playerDeltaPublisher),
			farmrpc.WithStealHintWriter(storage),
			farmrpc.WithTaskProgressWriter(storage),
			farmrpc.WithMailClaimer(storage),
		)
		mux := http.NewServeMux()
		mux.Handle("/internal/v1/cmd", rpcHandler)
		if os.Getenv("FARM_ALLOW_DEBUG_TIME") == "1" {
			mux.Handle("/internal/v1/debug/advance", authorizeBearer(config.internalToken, clock.AdvanceHandler()))
			logger.Info("debug time advance enabled", "component", "main", "op", "enable_debug_time", "role", roleFarm)
		}
		handler = mux
	default:
		return fmt.Errorf("unsupported FARM_ROLE %q", config.role)
	}

	server := &http.Server{
		Addr:              config.httpAddr,
		Handler:           metrics.InstrumentHandler(handler),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()
	probe.MarkReady()
	logger.Info("farm-server listening",
		"component", "main",
		"op", "listen_http",
		"role", config.role,
		"addr", config.httpAddr,
	)

	select {
	case err := <-serverErr:
		probe.BeginDrain()
		drainActors(farmRuntime)
		shutdownAdmin(admin)
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	case <-ctx.Done():
		// 关停顺序：readiness 先置 false → 停业务 HTTP → 疏散 Actor → 最后停 admin。
		// 探针不得反过来阻塞关停。
		probe.BeginDrain()
		logger.Info("shutdown begin", "component", "main", "op", "shutdown")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		httpErr := server.Shutdown(shutdownCtx)
		serveErr := <-serverErr

		drainActors(farmRuntime)
		shutdownAdmin(admin)

		if httpErr != nil {
			return fmt.Errorf("shutdown HTTP: %w", httpErr)
		}
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP after shutdown: %w", serveErr)
		}
		logger.Info("shutdown complete", "component", "main", "op", "shutdown")
		return nil
	}
}

func shutdownAdmin(admin *obs.Admin) {
	if admin == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := admin.Shutdown(ctx); err != nil {
		obs.L().Error("shutdown admin failed", "component", "main", "op", "shutdown_admin", "err", err.Error())
	}
}

// drainActors 把驻留 Actor 的内存权威写回存储。
//
// Actor 只在空闲 10 分钟或写回周期到点时落盘，而在线玩家的 Actor 永不空闲：少了这
// 一步，一次正常发布就会丢掉上个写回周期之后的全部改动。
func drainActors(runtime *actor.Runtime) {
	if runtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), actorDrainTimeout)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		obs.L().Error("drain actors failed", "component", "main", "op", "drain_actors", "err", err.Error())
	}
}

func newGateway(config config, storage *store.Store, runtime gateway.FarmRuntime, metrics *obs.Metrics, options ...gateway.Option) *gateway.Gateway {
	baseOptions := []gateway.Option{
		gateway.WithFriendStore(storage),
		gateway.WithStealHintStore(storage),
		gateway.WithTaskMailStore(storage),
		gateway.WithInviteSecret([]byte(config.inviteSecret)),
		gateway.WithConnectionRegistry(storage.ConnectionRegistry(), config.instanceID),
		gateway.WithInternalPushToken(config.internalToken),
		gateway.WithMetrics(metrics),
	}
	return gateway.New(auth.New(storage, storage), storage, runtime, append(baseOptions, options...)...)
}

func combineHandlers(public http.Handler, internal http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/internal/v1/cmd", internal)
	mux.Handle("/", public)
	return mux
}

func authorizeBearer(token string, next http.HandlerFunc) http.Handler {
	want := "Bearer " + strings.TrimSpace(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
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
	tokenSecret := getenv("FARM_TOKEN_SECRET", devTokenSecret)
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
	hazardSecret := getenv("FARM_HAZARD_SECRET", devHazardSecret)
	adminAddr, adminEnabled := obs.ParseAdminAddr(os.Getenv("FARM_ADMIN_ADDR"))
	cfg := config{
		env:           strings.ToLower(getenv("FARM_ENV", envDev)),
		httpAddr:      getenv("FARM_HTTP_ADDR", ":9002"),
		adminAddr:     adminAddr,
		adminEnabled:  adminEnabled,
		mysqlDSN:      getenv("FARM_MYSQL_DSN", "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"),
		redisAddr:     getenv("FARM_REDIS_ADDR", "127.0.0.1:6379"),
		tokenSecret:   tokenSecret,
		inviteSecret:  getenv("FARM_INVITE_SECRET", tokenSecret),
		hazardSecret:  hazardSecret,
		hazardSalt:    farm.DeriveHazardSalt(hazardSecret),
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
	if err := cfg.checkSecrets(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// checkSecrets 阻止把开发占位密钥带进非 dev 环境。
//
// 默认值让本地起服免配置，但它同时也是「忘了配」的静默结果——tokenSecret 用于签发
// 会话 token，泄露等于任何人都能伪造任意玩家的登录态。宁可启动失败，也不能带着
// 一个公开在源码里的密钥对外服务。
func (c config) checkSecrets() error {
	if c.env == envDev {
		return nil
	}

	var problems []string
	check := func(name, value string) {
		switch {
		case value == "" || value == devTokenSecret:
			problems = append(problems, name+" 仍是空值或开发占位值")
		case len(value) < minSecretLength:
			problems = append(problems, fmt.Sprintf("%s 长度 %d 不足 %d", name, len(value), minSecretLength))
		}
	}
	check("FARM_TOKEN_SECRET", c.tokenSecret)
	check("FARM_INVITE_SECRET", c.inviteSecret)
	// 单进程 all 模式没有跨进程调用，不强制内部令牌。
	if c.role != roleAll {
		check("FARM_INTERNAL_TOKEN", c.internalToken)
	}
	// gateway-only 不推进农场，可不配草/虫盐；all / farm 必须配置。
	if c.role == roleAll || c.role == roleFarm {
		switch {
		case c.hazardSecret == "" || c.hazardSecret == devHazardSecret:
			problems = append(problems, "FARM_HAZARD_SECRET 仍是空值或开发占位值")
		case len(c.hazardSecret) < minSecretLength:
			problems = append(problems, fmt.Sprintf("FARM_HAZARD_SECRET 长度 %d 不足 %d", len(c.hazardSecret), minSecretLength))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("FARM_ENV=%s 要求配置强密钥：%s", c.env, strings.Join(problems, "；"))
	}
	return nil
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
