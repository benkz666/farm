// Command benchfixture creates a fresh credential pool for isolated performance tests.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/gameconfig"
	"farm/server/shared/store"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

type authResponse struct {
	UID   string `json:"uid"`
	Token string `json:"token"`
	WSURL string `json:"ws_url"`
}

type fixture struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	UID          string `json:"uid"`
	Token        string `json:"token"`
	WSURL        string `json:"ws_url"`
	OwnerUID     string `json:"owner_uid"`
	PeerUID      string `json:"peer_uid"`
	PeerUsername string `json:"peer_username"`
	FromUID      string `json:"from_uid"`
	PlotIndex    int    `json:"plot_index"`
	CropID       int    `json:"crop_id"`
	ItemID       int    `json:"item_id"`
	Quantity     int    `json:"quantity"`
	TaskID       int    `json:"task_id"`
	MailID       string `json:"mail_id"`
}

func main() {
	baseURL := flag.String("base-url", "http://127.0.0.1:9002", "Gateway HTTP base URL")
	count := flag.Int("count", 16, "number of independent accounts")
	prefix := flag.String("prefix", "bench", "username prefix")
	password := flag.String("password", "bench-password-123!", "account password")
	output := flag.String("output", "accounts.json", "fixture output path")
	concurrency := flag.Int("concurrency", 32, "parallel account registrations")
	mergeBase := flag.String("merge-base", "", "optional first fixture in merge-only mode")
	mergeInput := flag.String("merge-input", "", "optional second fixture in merge-only mode")
	mysqlDSN := flag.String("mysql-dsn", "", "optional direct fixture MySQL DSN; skips password hashing and HTTP registration")
	redisAddr := flag.String("redis-addr", "", "Redis address required with mysql-dsn")
	uidBase := flag.Uint64("uid-base", 1_000_000, "first UID in direct fixture mode")
	wsURL := flag.String("ws-url", "ws://gateway:9002/ws", "WebSocket URL written in direct fixture mode")
	loginCapable := flag.Bool("login-capable", false, "store one reusable bcrypt hash for direct fixtures so /api/login can authenticate them")
	profile := flag.String("profile", "default", "direct fixture state: default, water, water-visitor, harvest, sell, or steal")
	flag.Parse()
	if *mergeBase != "" || *mergeInput != "" {
		if *mergeBase == "" || *mergeInput == "" {
			fatalf("merge-base and merge-input must be set together")
		}
		if err := mergeFixtures(*mergeBase, *mergeInput, *output); err != nil {
			fatalf("merge fixtures: %v", err)
		}
		return
	}
	if *count < 2 || *concurrency < 1 {
		fatalf("count must be at least 2")
	}
	if (*mysqlDSN == "") != (*redisAddr == "") {
		fatalf("mysql-dsn and redis-addr must be set together")
	}
	if !validFixtureProfile(*profile) {
		fatalf("unknown profile %q", *profile)
	}
	if *profile != "default" && *mysqlDSN == "" {
		fatalf("stateful profile %q requires direct mysql-dsn/redis-addr mode", *profile)
	}

	var directStorage *store.Store
	var closeStorage func() error
	var directPasswordHash string
	if *mysqlDSN != "" {
		var err error
		directStorage, closeStorage, err = store.Open(context.Background(), *mysqlDSN, *redisAddr, 0)
		if err != nil {
			fatalf("open direct fixture storage: %v", err)
		}
		defer closeStorage()
		if *loginCapable {
			hash, hashErr := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
			if hashErr != nil {
				fatalf("hash direct fixture password: %v", hashErr)
			}
			directPasswordHash = string(hash)
		}
	}

	runSuffix := time.Now().UTC().Format("150405")
	fixtures := make([]fixture, *count)
	jobs := make(chan int)
	errs := make(chan error, *concurrency)
	var completed atomic.Int64
	var workers sync.WaitGroup
	// Stateful profiles rewrite normalized Farm rows. MySQL can otherwise take
	// overlapping next-key locks while many fixtures delete/insert adjacent UID
	// ranges, causing preparation-only deadlocks. Fixture generation is outside
	// the measurement window, so serialize this small state materialization step
	// while keeping account/session creation parallel.
	var statefulPrepareMu sync.Mutex
	for range *concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				username := fmt.Sprintf("%s%s%03d", *prefix, runSuffix, index)
				if len(username) > 32 {
					errs <- fmt.Errorf("generated username exceeds 32 bytes: %s", username)
					continue
				}
				var auth authResponse
				var err error
				if directStorage != nil {
					uid := *uidBase + uint64(index)
					auth, err = createDirectFixture(directStorage, uid, username, *wsURL, directPasswordHash)
					if err == nil {
						if *profile != "default" {
							statefulPrepareMu.Lock()
							err = prepareDirectFixtureWithRetry(context.Background(), directStorage, uid, *profile)
							statefulPrepareMu.Unlock()
						} else {
							err = prepareDirectFixtureWithRetry(context.Background(), directStorage, uid, *profile)
						}
					}
				} else {
					auth, err = register(strings.TrimRight(*baseURL, "/"), username, *password)
				}
				if err != nil {
					errs <- fmt.Errorf("register %s: %w", username, err)
					continue
				}
				fixtures[index] = fixture{
					Username: username, Password: *password, UID: auth.UID, Token: auth.Token, WSURL: auth.WSURL,
					OwnerUID: "0", PlotIndex: 0, CropID: 1, ItemID: 1, Quantity: 1, TaskID: 1, MailID: "1",
				}
				if done := completed.Add(1); done%250 == 0 {
					fmt.Printf("benchfixture: registered %d/%d\n", done, *count)
				}
			}
		}()
	}
	go func() {
		for index := 0; index < *count; index++ {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(errs)
	}()
	for err := range errs {
		fatalf("%v", err)
	}
	for index := range fixtures {
		peer := fixtures[(index+1)%len(fixtures)]
		fixtures[index].PeerUID = peer.UID
		fixtures[index].PeerUsername = peer.Username
		fixtures[index].FromUID = peer.UID
		if *profile == "water-visitor" {
			fixtures[index].OwnerUID = peer.UID
		}
	}
	if directStorage != nil && (*profile == "steal" || *profile == "water-visitor") {
		seen := make(map[[2]uint64]struct{}, len(fixtures))
		for _, item := range fixtures {
			uid, err := strconv.ParseUint(item.UID, 10, 64)
			if err != nil {
				fatalf("parse fixture uid %q: %v", item.UID, err)
			}
			peerUID, err := strconv.ParseUint(item.PeerUID, 10, 64)
			if err != nil {
				fatalf("parse peer uid %q: %v", item.PeerUID, err)
			}
			pair := [2]uint64{uid, peerUID}
			if pair[0] > pair[1] {
				pair[0], pair[1] = pair[1], pair[0]
			}
			if _, ok := seen[pair]; ok {
				continue
			}
			seen[pair] = struct{}{}
			if err := directStorage.AddFriends(context.Background(), uid, peerUID); err != nil && !errors.Is(err, store.ErrAlreadyFriend) {
				fatalf("prepare steal friendship %d/%d: %v", uid, peerUID, err)
			}
		}
	}

	file, err := os.Create(*output)
	if err != nil {
		fatalf("create output: %v", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{"accounts": fixtures}); err != nil {
		_ = file.Close()
		fatalf("encode output: %v", err)
	}
	if err := file.Close(); err != nil {
		fatalf("close output: %v", err)
	}
	fmt.Printf("benchfixture: generated %d accounts in %s\n", len(fixtures), *output)
}

func validFixtureProfile(profile string) bool {
	switch profile {
	case "default", "water", "water-visitor", "harvest", "sell", "steal":
		return true
	default:
		return false
	}
}

func prepareDirectFixture(ctx context.Context, storage *store.Store, uid uint64, profile string) error {
	if profile == "default" {
		return nil
	}
	aggregate, err := storage.LoadFarm(ctx, uid)
	if err != nil {
		return fmt.Errorf("load direct fixture farm: %w", err)
	}
	crop, ok := gameconfig.CropByID(1)
	if !ok {
		return errors.New("crop 1 is not configured")
	}
	now := time.Now().UnixMilli()
	stageCount := uint8(3)
	if crop.UnlockLevel >= 3 {
		stageCount = 4
	}
	switch profile {
	case "water", "water-visitor":
		// Direct fixture creation can take tens of seconds for a large account
		// pool, while the demo profile may mature a crop sooner than that. Keep
		// the fixture growing until its first EnterFarm; Farm then reprofiles it
		// to the authoritative server profile before the measured Water action.
		duration := int64((15 * time.Minute) / time.Millisecond)
		aggregate.Plots[0] = farm.Plot{
			State:          farm.StateGrowing,
			SeasonTotal:    crop.Seasons,
			StageCount:     stageCount,
			CropID:         crop.ID,
			PlantNonce:     1,
			SeasonStartAt:  now,
			SeasonDuration: duration,
			MatureAt:       now + duration,
			LastSettleAt:   now,
			LastWaterAt:    0,
		}
	case "harvest", "steal":
		aggregate.Plots[0] = farm.Plot{
			State:          farm.StateMature,
			SeasonTotal:    crop.Seasons,
			StageCount:     stageCount,
			CropID:         crop.ID,
			FinalYield:     crop.Yield,
			PlantNonce:     1,
			HarvestRound:   1,
			SeasonStartAt:  now - 1,
			SeasonDuration: 1,
			MatureAt:       now - 1,
			LastSettleAt:   now - 1,
		}
	case "sell":
		aggregate.AddItem(farm.FruitItem(crop.ID), 100)
	}
	aggregate.FarmSeq++
	if err := storage.SaveFarm(ctx, aggregate); err != nil {
		return fmt.Errorf("save %s fixture farm: %w", profile, err)
	}
	return nil
}

func prepareDirectFixtureWithRetry(ctx context.Context, storage *store.Store, uid uint64, profile string) error {
	const attempts = 5
	for attempt := 0; attempt < attempts; attempt++ {
		err := prepareDirectFixture(ctx, storage, uid, profile)
		if err == nil {
			return nil
		}
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1213 && mysqlErr.Number != 1205 {
			return err
		}
		if attempt+1 == attempts {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func createDirectFixture(storage *store.Store, uid uint64, username, wsURL, passwordHash string) (authResponse, error) {
	if storage == nil || uid == 0 || wsURL == "" {
		return authResponse{}, fmt.Errorf("invalid direct fixture arguments")
	}
	// Token-only fixtures use an inert marker. Login-capable fixtures reuse one
	// valid bcrypt hash: the expensive hash generation remains outside account
	// creation and the measurement window, while Login still performs the real
	// bcrypt comparison for every request.
	if passwordHash == "" {
		passwordHash = "direct-performance-fixture"
	}
	if err := storage.SaveAccount(context.Background(), uid, username, passwordHash); err != nil {
		return authResponse{}, err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return authResponse{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	if err := storage.Put(context.Background(), token, uid, 7*24*time.Hour); err != nil {
		return authResponse{}, err
	}
	return authResponse{UID: fmt.Sprint(uid), Token: token, WSURL: wsURL}, nil
}

func mergeFixtures(firstPath, secondPath, outputPath string) error {
	var merged struct {
		Accounts []fixture `json:"accounts"`
	}
	for _, path := range []string{firstPath, secondPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var input struct {
			Accounts []fixture `json:"accounts"`
		}
		if err := json.Unmarshal(data, &input); err != nil {
			return err
		}
		merged.Accounts = append(merged.Accounts, input.Accounts...)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(merged); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	fmt.Printf("benchfixture: merged %d accounts into %s\n", len(merged.Accounts), outputPath)
	return nil
}

func register(baseURL, username, password string) (authResponse, error) {
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return authResponse{}, err
	}
	response, err := http.Post(baseURL+"/api/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return authResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return authResponse{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var result authResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return authResponse{}, err
	}
	if result.UID == "" || result.Token == "" || result.WSURL == "" {
		return authResponse{}, fmt.Errorf("incomplete auth response")
	}
	return result, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "benchfixture: "+format+"\n", args...)
	os.Exit(1)
}
