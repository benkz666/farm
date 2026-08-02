// farm 是用户 Actor 与农场权威状态的唯一部署入口。
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"farm/server/api/socialapi"
	"farm/server/api/workerapi"
	"farm/server/platform/actor"
	"farm/server/platform/cross"
	"farm/server/platform/debugclock"
	"farm/server/platform/farm"
	"farm/server/platform/farmrpc"
	"farm/server/platform/gameconf"
	"farm/server/platform/obs"
	"farm/server/platform/servicehost"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := servicehost.LoadConfig("farm", ":9100", "127.0.0.1:9310")
	if err != nil {
		return err
	}
	instanceID := servicehost.Getenv("FARM_FARM_INSTANCE_ID", "farm-0")
	routes, err := servicehost.LoadRouteTable(
		servicehost.Getenv("FARM_ROUTE_TABLE", "deploy/route-table.local.json"),
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
		return fmt.Errorf("farm: unsupported FARM_TIME_PROFILE %q", timeProfile)
	}
	hazardSecret := servicehost.Getenv("FARM_HAZARD_SECRET", "dev-only-hazard-secret")
	if config.Environment != "dev" && len(hazardSecret) < 32 {
		return fmt.Errorf("farm: FARM_HAZARD_SECRET must contain at least 32 bytes outside dev")
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
	runtime := actor.NewRuntime(storage, 0)
	runtime.SetHazardSalt(farm.DeriveHazardSalt(hazardSecret))
	runtime.SetMetrics(metrics)
	owns := func(uid uint64) bool {
		farmID, routeErr := routes.FarmID(uid)
		return routeErr == nil && farmID == instanceID
	}

	deltaPublisher := farmrpc.NewFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewHTTPDeltaPusher(gatewayURLs, config.InternalToken),
	)
	deltaPublisher.SetMetrics(metrics)
	playerDeltaPublisher := farmrpc.NewPlayerFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewHTTPPlayerDeltaPusher(gatewayURLs, config.InternalToken),
	)
	taskNotifyPublisher := farmrpc.NewTaskFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewHTTPTaskNotifyPusher(gatewayURLs, config.InternalToken),
	)
	mailNotifyPublisher := farmrpc.NewMailFanoutPublisher(
		storage.ConnectionRegistry(),
		farmrpc.NewHTTPMailNotifyPusher(gatewayURLs, config.InternalToken),
	)
	workerClient := workerapi.NewClient(workerURL, config.InternalToken)
	socialClient := socialapi.NewClient(socialURL, config.InternalToken)
	clock := &debugclock.Clock{}
	owner := cross.NewOwner(runtime, socialClient, eventBus, clock.Now, deltaPublisher, owns)
	owner.SetStealHintWriter(storage)
	owner.SetPlayerDeltaPublisher(playerDeltaPublisher)
	if err := owner.Start(ctx); err != nil {
		return fmt.Errorf("farm: start cross-owner consumer: %w", err)
	}

	timeProfiles := gameconf.NewTimeProfileSwitch(timeProfile)
	commandHandler := farmrpc.NewHandler(
		runtime,
		[]byte(config.InternalToken),
		owns,
		clock.Now,
		farmrpc.WithDeltaPublisher(deltaPublisher),
		farmrpc.WithPlayerDeltaPublisher(playerDeltaPublisher),
		farmrpc.WithTaskNotifyPublisher(taskNotifyPublisher),
		farmrpc.WithStealHintWriter(storage),
		farmrpc.WithTaskProgressWriter(workerClient),
		farmrpc.WithTaskClaimer(workerClient),
		farmrpc.WithDailyLoginClaimer(workerClient),
		farmrpc.WithMailClaimer(workerClient),
		farmrpc.WithCodexRewardStore(workerClient),
		farmrpc.WithMailNotifyPublisher(mailNotifyPublisher),
		farmrpc.WithTimeProfileSwitch(timeProfiles),
	)
	mux := http.NewServeMux()
	mux.Handle("/internal/v1/cmd", commandHandler)
	if servicehost.Getenv("FARM_ALLOW_DEBUG_TIME", "0") == "1" {
		mux.Handle("/internal/v1/debug/advance", authorize(config.InternalToken, clock.AdvanceHandler()))
		mux.Handle("/internal/v1/debug/time-profile", authorize(
			config.InternalToken,
			timeProfileHandler(timeProfiles),
		))
	}

	return (servicehost.Host{
		Config:         config,
		Handler:        mux,
		Checker:        obs.FuncChecker("storage", storage.Ping),
		Metrics:        metrics,
		BeforeShutdown: runtime.Shutdown,
	}).Run(ctx)
}

func authorize(internalToken string, next http.HandlerFunc) http.Handler {
	want := []byte("Bearer " + strings.TrimSpace(internalToken))
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")), want) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, request)
	})
}

func timeProfileHandler(profiles *gameconf.TimeProfileSwitch) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var input struct {
			TimeProfile string `json:"time_profile"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || !profiles.Set(input.TimeProfile) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"time_profile": profiles.Get()})
	}
}
