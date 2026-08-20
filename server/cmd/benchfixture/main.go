// Command benchfixture creates a fresh credential pool for isolated performance tests.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
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
	PlotIndexes  []int  `json:"plot_indexes,omitempty"`
	CropID       int    `json:"crop_id"`
	ItemID       int    `json:"item_id"`
	Quantity     int    `json:"quantity"`
	TaskID       int    `json:"task_id"`
	TaskIDs      []int  `json:"task_ids,omitempty"`
	MailID       string `json:"mail_id"`
	MailReadID   string `json:"mail_read_id,omitempty"`
	MailClaimID  string `json:"mail_claim_id,omitempty"`
	MailDeleteID string `json:"mail_delete_id,omitempty"`
}

type fixtureFile struct {
	TimeProfile string    `json:"time_profile,omitempty"`
	Accounts    []fixture `json:"accounts"`
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
	normalizeMixedInput := flag.String("normalize-mixed-input", "", "copy an existing fixture and normalize mixed-pool peer relationships")
	accountOffset := flag.Int("account-offset", 0, "skip this many accounts when copying or resetting a fixture")
	accountLimit := flag.Int("account-limit", 0, "maximum accounts to copy or reset; zero uses all remaining accounts")
	resetInput := flag.String("reset-input", "", "reuse accounts from an existing fixture and reset their Farm state")
	mysqlDSN := flag.String("mysql-dsn", "", "optional direct fixture MySQL DSN; skips password hashing and HTTP registration")
	redisAddr := flag.String("redis-addr", "", "Redis address required with mysql-dsn")
	uidBase := flag.Uint64("uid-base", 1_000_000, "first UID in direct fixture mode")
	wsURL := flag.String("ws-url", "ws://gateway:9002/ws", "WebSocket URL written in direct fixture mode")
	loginCapable := flag.Bool("login-capable", false, "store one reusable bcrypt hash for direct fixtures so /api/login can authenticate them")
	profile := flag.String("profile", "default", "direct fixture state: default, water, water-visitor, harvest, sell, hot-economy, steal, or mixed")
	timeProfile := flag.String("time-profile", fixtureTimeProfileDefault(), "Farm time profile used to build stateful fixtures; must match farmsvr")
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
	if *normalizeMixedInput != "" {
		input, err := readFixtureFile(*normalizeMixedInput)
		if err != nil {
			fatalf("read mixed fixture: %v", err)
		}
		input, err = selectFixtureAccounts(input, *accountOffset, *accountLimit)
		if err != nil {
			fatalf("select mixed fixture accounts: %v", err)
		}
		assignMixedPeers(input.Accounts)
		if err := writeFixtures(*output, input); err != nil {
			fatalf("write normalized mixed fixture: %v", err)
		}
		fmt.Printf("benchfixture: normalized %d mixed accounts into %s\n", len(input.Accounts), *output)
		return
	}
	if *resetInput != "" && (*mysqlDSN == "" || *redisAddr == "") {
		fatalf("reset-input requires mysql-dsn and redis-addr")
	}
	if *resetInput != "" && *profile == "default" {
		fatalf("reset-input requires a stateful profile")
	}
	if *count < 2 || *concurrency < 1 {
		fatalf("count must be at least 2")
	}
	if *accountOffset < 0 || *accountLimit < 0 {
		fatalf("account-offset and account-limit must not be negative")
	}
	if (*mysqlDSN == "") != (*redisAddr == "") {
		fatalf("mysql-dsn and redis-addr must be set together")
	}
	if !validFixtureProfile(*profile) {
		fatalf("unknown profile %q", *profile)
	}
	if !gameconfig.ValidTimeProfile(*timeProfile) {
		fatalf("unknown time profile %q", *timeProfile)
	}
	if *profile != "default" && *mysqlDSN == "" {
		fatalf("stateful profile %q requires direct mysql-dsn/redis-addr mode", *profile)
	}

	var directStorage *store.Store
	var closeStorage func() error
	var directDB *sql.DB
	var directPasswordHash string
	if *mysqlDSN != "" {
		var err error
		directStorage, closeStorage, err = store.Open(context.Background(), *mysqlDSN, *redisAddr, 0)
		if err != nil {
			fatalf("open direct fixture storage: %v", err)
		}
		defer closeStorage()
		directDB, err = sql.Open("mysql", *mysqlDSN)
		if err != nil {
			fatalf("open fixture SQL: %v", err)
		}
		defer directDB.Close()
		if err := directDB.PingContext(context.Background()); err != nil {
			fatalf("ping fixture SQL: %v", err)
		}
		if *loginCapable {
			hash, hashErr := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
			if hashErr != nil {
				fatalf("hash direct fixture password: %v", hashErr)
			}
			directPasswordHash = string(hash)
		}
	}
	if *resetInput != "" {
		resetFile, err := readFixtureFile(*resetInput)
		if err != nil {
			fatalf("read reset fixture: %v", err)
		}
		resetFile, err = selectFixtureAccounts(resetFile, *accountOffset, *accountLimit)
		if err != nil {
			fatalf("select reset fixture accounts: %v", err)
		}
		if err := resetFixtures(directStorage, directDB, resetFile.Accounts, *profile, *timeProfile, *concurrency); err != nil {
			fatalf("reset fixtures: %v", err)
		}
		if *profile == "mixed" {
			// Older reusable fixture files only contained a single task_id/mail_id.
			// Reset now discovers every current-day task and the three independent
			// mail IDs, so persist that metadata before servicebench consumes it.
			// Without this refresh, a second TaskClaim cycle falls back to task 4
			// and reports a false ERR_TASK_ALREADY_CLAIMED saturation signal.
			resetFile.TimeProfile = *timeProfile
			if err := writeFixtures(*resetInput, resetFile); err != nil {
				fatalf("refresh mixed reset fixture metadata: %v", err)
			}
		}
		fmt.Printf("benchfixture: reset %d existing accounts from %s with profile %s\n", len(resetFile.Accounts), *resetInput, *profile)
		return
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
				var taskIDs []int
				var mailReadID, mailClaimID, mailDeleteID string
				var err error
				if directStorage != nil {
					uid := *uidBase + uint64(index)
					auth, err = createDirectFixture(directStorage, uid, username, *wsURL, directPasswordHash)
					if err == nil {
						if *profile != "default" {
							statefulPrepareMu.Lock()
							err = prepareDirectFixtureWithRetry(context.Background(), directStorage, uid, *profile, *timeProfile)
							statefulPrepareMu.Unlock()
						} else {
							err = prepareDirectFixtureWithRetry(context.Background(), directStorage, uid, *profile, *timeProfile)
						}
						if err == nil && *profile == "mixed" {
							err = withDeadlockRetry(context.Background(), func() error {
								var auxiliaryErr error
								taskIDs, mailReadID, mailClaimID, mailDeleteID, auxiliaryErr = prepareMixedAuxiliary(
									context.Background(), directStorage, directDB, uid,
								)
								return auxiliaryErr
							})
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
					OwnerUID: "0", PlotIndex: 0, PlotIndexes: fixturePlotIndexes(*profile),
					CropID: 1, ItemID: 1, Quantity: 1, TaskID: 4, TaskIDs: taskIDs,
					MailID: mailReadID, MailReadID: mailReadID, MailClaimID: mailClaimID, MailDeleteID: mailDeleteID,
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
	if *profile == "mixed" {
		assignMixedPeers(fixtures)
	} else {
		for index := range fixtures {
			peer := fixtures[(index+1)%len(fixtures)]
			fixtures[index].PeerUID = peer.UID
			fixtures[index].PeerUsername = peer.Username
			fixtures[index].FromUID = peer.UID
			if *profile == "water-visitor" {
				fixtures[index].OwnerUID = peer.UID
			}
		}
	}
	if directStorage != nil && (*profile == "steal" || *profile == "water-visitor" || *profile == "mixed") {
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

	if err := writeFixtures(*output, fixtureFile{TimeProfile: *timeProfile, Accounts: fixtures}); err != nil {
		fatalf("write output: %v", err)
	}
	fmt.Printf("benchfixture: generated %d accounts in %s\n", len(fixtures), *output)
}

func assignMixedPeers(fixtures []fixture) {
	if len(fixtures) < 2 {
		return
	}
	localEnd := len(fixtures) * 3 / 5
	visitorCount := len(fixtures) - localEnd
	for index := range fixtures {
		peerIndex := (index + 1) % max(localEnd, 1)
		if index >= localEnd && visitorCount > 1 {
			peerIndex = localEnd + (index-localEnd+1)%visitorCount
		}
		peer := fixtures[peerIndex]
		fixtures[index].PeerUID = peer.UID
		fixtures[index].PeerUsername = peer.Username
		fixtures[index].FromUID = peer.UID
	}
}

func writeFixtures(path string, value fixtureFile) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func validFixtureProfile(profile string) bool {
	switch profile {
	case "default", "water", "water-visitor", "harvest", "sell", "hot-economy", "steal", "mixed":
		return true
	default:
		return false
	}
}

func fixtureTimeProfileDefault() string {
	profile := strings.ToLower(strings.TrimSpace(os.Getenv("FARM_TIME_PROFILE")))
	if !gameconfig.ValidTimeProfile(profile) {
		return gameconfig.TimeProfileDemo
	}
	return profile
}

func fixturePlotIndexes(profile string) []int {
	switch profile {
	case "water", "water-visitor", "harvest", "steal", "mixed":
		indexes := make([]int, gameconfig.MaxPlots)
		for index := range indexes {
			indexes[index] = index
		}
		return indexes
	default:
		return nil
	}
}

func readFixtures(path string) ([]fixture, error) {
	decoded, err := readFixtureFile(path)
	if err != nil {
		return nil, err
	}
	return decoded.Accounts, nil
}

func readFixtureFile(path string) (fixtureFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fixtureFile{}, err
	}
	var decoded fixtureFile
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fixtureFile{}, err
	}
	if len(decoded.Accounts) < 2 {
		return fixtureFile{}, errors.New("fixture requires at least two accounts")
	}
	return decoded, nil
}

func selectFixtureAccounts(input fixtureFile, offset, limit int) (fixtureFile, error) {
	if offset < 0 || limit < 0 {
		return fixtureFile{}, errors.New("fixture offset and limit must not be negative")
	}
	if offset >= len(input.Accounts) {
		return fixtureFile{}, fmt.Errorf("fixture offset %d exceeds %d accounts", offset, len(input.Accounts))
	}
	end := len(input.Accounts)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	selected := append([]fixture(nil), input.Accounts[offset:end]...)
	if len(selected) < 2 {
		return fixtureFile{}, errors.New("selected fixture requires at least two accounts")
	}
	input.Accounts = selected
	return input, nil
}

func resetFixtures(storage *store.Store, directDB *sql.DB, fixtures []fixture, profile, timeProfile string, concurrency int) error {
	if storage == nil {
		return errors.New("reset fixture storage is nil")
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	concurrency = min(concurrency, len(fixtures))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := make(chan int)
	errs := make(chan error, concurrency)
	var completed atomic.Int64
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				item := &fixtures[index]
				uid, err := strconv.ParseUint(item.UID, 10, 64)
				if err != nil || uid == 0 {
					errs <- fmt.Errorf("invalid fixture uid %q", item.UID)
					cancel()
					return
				}
				if item.Token == "" {
					errs <- fmt.Errorf("fixture uid %d has an empty token", uid)
					cancel()
					return
				}
				// The account pool is permanent; refreshing its existing session keeps
				// repeated benchmark runs from needing account regeneration after the
				// normal seven-day token TTL.
				if err := storage.Put(ctx, item.Token, uid, 7*24*time.Hour); err != nil {
					errs <- fmt.Errorf("refresh fixture session %d: %w", uid, err)
					cancel()
					return
				}
				if err := prepareDirectFixtureWithRetry(ctx, storage, uid, profile, timeProfile); err != nil {
					errs <- err
					cancel()
					return
				}
				item.PlotIndexes = fixturePlotIndexes(profile)
				item.PlotIndex = 0
				item.CropID = 1
				item.ItemID = 1
				item.Quantity = 1
				if profile == "mixed" {
					var (
						taskIDs                               []int
						mailReadID, mailClaimID, mailDeleteID string
					)
					auxiliaryErr := withDeadlockRetry(ctx, func() error {
						var err error
						taskIDs, mailReadID, mailClaimID, mailDeleteID, err =
							prepareMixedAuxiliary(ctx, storage, directDB, uid)
						return err
					})
					if auxiliaryErr != nil {
						errs <- auxiliaryErr
						cancel()
						return
					}
					applyMixedAuxiliaryMetadata(item, taskIDs, mailReadID, mailClaimID, mailDeleteID)
				}
				if (profile == "steal" || profile == "water-visitor" || profile == "mixed") && item.PeerUID != "" {
					peerUID, parseErr := strconv.ParseUint(item.PeerUID, 10, 64)
					if parseErr != nil || peerUID == 0 {
						errs <- fmt.Errorf("invalid fixture peer uid %q", item.PeerUID)
						cancel()
						return
					}
					friendErr := withDeadlockRetry(ctx, func() error {
						err := storage.AddFriends(ctx, uid, peerUID)
						if errors.Is(err, store.ErrAlreadyFriend) {
							return nil
						}
						return err
					})
					if friendErr != nil {
						errs <- friendErr
						cancel()
						return
					}
				}
				if done := completed.Add(1); done%250 == 0 {
					fmt.Printf("benchfixture: reset %d/%d\n", done, len(fixtures))
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range fixtures {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	workers.Wait()
	close(errs)
	return <-errs
}

func applyMixedAuxiliaryMetadata(
	item *fixture,
	taskIDs []int,
	mailReadID, mailClaimID, mailDeleteID string,
) {
	if item == nil {
		return
	}
	item.TaskIDs = append(item.TaskIDs[:0], taskIDs...)
	if len(item.TaskIDs) > 0 {
		item.TaskID = item.TaskIDs[0]
	}
	item.MailReadID = mailReadID
	item.MailClaimID = mailClaimID
	item.MailDeleteID = mailDeleteID
	// Keep the legacy field useful for older single-mail benchmark commands.
	if item.MailID == "" {
		item.MailID = mailReadID
	}
}

func prepareDirectFixture(ctx context.Context, storage *store.Store, uid uint64, profile, timeProfile string) error {
	if profile == "default" {
		return nil
	}
	aggregate, err := storage.LoadFarm(ctx, uid)
	if err != nil {
		return fmt.Errorf("load direct fixture farm: %w", err)
	}
	aggregate = farm.NewAggregate(uid, aggregate.Nickname)
	if err := prepareAggregateProfile(aggregate, profile, timeProfile, time.Now().UnixMilli()); err != nil {
		return err
	}
	aggregate.FarmSeq++
	if err := storage.SaveFarm(ctx, aggregate); err != nil {
		return fmt.Errorf("save %s fixture farm: %w", profile, err)
	}
	return nil
}

func prepareAggregateProfile(aggregate *farm.Aggregate, profile, timeProfile string, now int64) error {
	if aggregate == nil || aggregate.UID == 0 {
		return errors.New("invalid fixture aggregate")
	}
	crop, ok := gameconfig.CropByID(1)
	if !ok {
		return errors.New("crop 1 is not configured")
	}
	stageCount := uint8(3)
	if crop.UnlockLevel >= 3 {
		stageCount = 4
	}
	switch profile {
	case "water", "water-visitor":
		// The duration must already match Farm's authoritative profile. Otherwise
		// EnterFarm reprofiles every plot during warmup and injects unrelated
		// journal/MySQL work into the measured Water window.
		duration := gameconfig.SeasonDurationMs(crop, 0, timeProfile)
		if duration <= 0 {
			return fmt.Errorf("invalid fixture time profile %q", timeProfile)
		}
		aggregate.UnlockedPlots = uint8(gameconfig.MaxPlots)
		for index := range aggregate.Plots {
			aggregate.Plots[index] = farm.Plot{
				State:          farm.StateGrowing,
				SeasonTotal:    crop.Seasons,
				StageCount:     stageCount,
				CropID:         crop.ID,
				PlantNonce:     uint32(index + 1),
				SeasonStartAt:  now,
				SeasonDuration: duration,
				MatureAt:       now + duration,
				LastSettleAt:   now,
				LastWaterAt:    0,
			}
		}
	case "harvest", "steal":
		aggregate.UnlockedPlots = uint8(gameconfig.MaxPlots)
		for index := range aggregate.Plots {
			aggregate.Plots[index] = farm.Plot{
				State:          farm.StateMature,
				SeasonTotal:    crop.Seasons,
				StageCount:     stageCount,
				CropID:         crop.ID,
				FinalYield:     crop.Yield,
				PlantNonce:     uint32(index + 1),
				HarvestRound:   1,
				SeasonStartAt:  now - 1,
				SeasonDuration: 1,
				MatureAt:       now - 1,
				LastSettleAt:   now - 1,
			}
		}
	case "sell":
		aggregate.AddItem(farm.FruitItem(crop.ID), 100)
	case "hot-economy":
		// Buy/Sell hot-path tests keep the WebSocket and Actor resident while
		// sending many commands. Seed enough assets outside the measurement
		// window so business rejections cannot be mistaken for saturation.
		aggregate.Coin = 1_000_000_000
		aggregate.AddItem(farm.FruitItem(crop.ID), 1_000_000)
	case "mixed":
		// One aggregate carries independent legal states for every local/cross
		// operation in the normal-v1 mix. Growing plots can each accept Water,
		// Weed, Pest and Fertilize once; the other state transitions use disjoint
		// plots so their benchmark arrivals do not invalidate one another.
		duration := gameconfig.SeasonDurationMs(crop, 0, timeProfile)
		if duration <= 0 {
			return fmt.Errorf("invalid fixture time profile %q", timeProfile)
		}
		aggregate.UnlockedPlots = uint8(gameconfig.MaxPlots)
		aggregate.Coin = 1_000_000_000
		aggregate.AddItem(farm.SeedItem(crop.ID), 1_000_000)
		aggregate.AddItem(farm.FertilizerItem(1), 1_000_000)
		aggregate.AddItem(farm.FruitItem(crop.ID), 1_000_000)
		aggregate.AddItem(farm.DogFoodItem(), 1_000_000)
		aggregate.Pet = farm.PetState{ActiveDog: farm.DogMutt, Owned: 0b111}
		for index := 0; index < 6; index++ {
			aggregate.Plots[index] = farm.Plot{
				State: farm.StateGrowing, SeasonTotal: crop.Seasons, StageCount: stageCount,
				CropID: crop.ID, PlantNonce: uint32(index + 1), SeasonStartAt: now,
				SeasonDuration: duration, MatureAt: now + duration, LastSettleAt: now,
				WeedSince: now - 1, PestSince: now - 1,
			}
		}
		for index := 6; index < 11; index++ {
			aggregate.Plots[index] = farm.Plot{
				State: farm.StateMature, SeasonTotal: crop.Seasons, StageCount: stageCount,
				CropID: crop.ID, FinalYield: crop.Yield, PlantNonce: uint32(index + 1),
				HarvestRound: 1, SeasonStartAt: now - 1, SeasonDuration: 1,
				MatureAt: now - 1, LastSettleAt: now - 1,
			}
		}
		for index := 11; index < 13; index++ {
			aggregate.Plots[index] = farm.NewWastelandPlot()
		}
		for index := 13; index < 15; index++ {
			aggregate.Plots[index] = farm.Plot{State: farm.StateResidue}
		}
		for index := 15; index < 18; index++ {
			aggregate.Plots[index] = farm.Plot{State: farm.StateTilled}
		}
	default:
		return fmt.Errorf("unsupported stateful fixture profile %q", profile)
	}
	return nil
}

func prepareMixedAuxiliary(
	ctx context.Context,
	storage *store.Store,
	directDB *sql.DB,
	uid uint64,
) (taskIDs []int, mailReadID, mailClaimID, mailDeleteID string, err error) {
	if storage == nil || directDB == nil || uid == 0 || uid > (^uint64(0)-3)/10 {
		return nil, "", "", "", errors.New("invalid mixed auxiliary fixture arguments")
	}
	dayKey := gameconfig.LocalDayKey(time.Now().UnixMilli())
	if _, err := directDB.ExecContext(ctx, `DELETE FROM player_task WHERE uid = ? AND logic_day = ?`, uid, dayKey); err != nil {
		return nil, "", "", "", fmt.Errorf("reset mixed tasks %d: %w", uid, err)
	}
	tasks, err := storage.ListTasks(ctx, uid, dayKey)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("list mixed tasks %d: %w", uid, err)
	}
	taskIDs = make([]int, 0, len(tasks))
	for _, task := range tasks {
		if _, err := storage.AdvanceTask(ctx, uid, dayKey, task.ID, max(task.Target, 1)); err != nil {
			return nil, "", "", "", fmt.Errorf("seed mixed task %d/%d: %w", uid, task.ID, err)
		}
		taskIDs = append(taskIDs, int(task.ID))
	}

	readID, claimID, deleteID := uid*10+1, uid*10+2, uid*10+3
	if _, err := directDB.ExecContext(ctx, `DELETE FROM mail WHERE uid = ?`, uid); err != nil {
		return nil, "", "", "", fmt.Errorf("reset mixed mails %d: %w", uid, err)
	}
	now := time.Now().UnixMilli()
	if _, err := directDB.ExecContext(ctx, `
		INSERT INTO mail (id, uid, title, attachment_coin, claimed_at, read_at, created_at)
		VALUES (?, ?, 'mixed-read', 0, NULL, NULL, ?),
		       (?, ?, 'mixed-claim', 100, NULL, NULL, ?),
		       (?, ?, 'mixed-delete', 0, NULL, NULL, ?)`,
		readID, uid, now, claimID, uid, now+1, deleteID, uid, now+2,
	); err != nil {
		return nil, "", "", "", fmt.Errorf("seed mixed mails %d: %w", uid, err)
	}
	// Marking once goes through Store's real cross-process invalidation channel.
	// The direct UPDATE then restores unread fixture state without leaving an old
	// MailList value in either Farm's local cache or Redis's versioned cache.
	if _, err := storage.MarkMailsRead(ctx, uid, 0); err != nil {
		return nil, "", "", "", fmt.Errorf("invalidate mixed mailbox %d: %w", uid, err)
	}
	if _, err := directDB.ExecContext(ctx, `UPDATE mail SET read_at = NULL WHERE uid = ?`, uid); err != nil {
		return nil, "", "", "", fmt.Errorf("restore mixed mail state %d: %w", uid, err)
	}
	return taskIDs,
		strconv.FormatUint(readID, 10),
		strconv.FormatUint(claimID, 10),
		strconv.FormatUint(deleteID, 10),
		nil
}

// 并发重置会让多个 worker 在相邻主键上交错加锁，MySQL 报 1213 死锁或 1205 锁
// 等待超时。每个准备步骤都以删除该 uid 的旧行开始，所以整段重跑是幂等的。
func withDeadlockRetry(ctx context.Context, run func() error) error {
	const attempts = 5
	for attempt := 0; attempt < attempts; attempt++ {
		err := run()
		if err == nil {
			return nil
		}
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || (mysqlErr.Number != 1213 && mysqlErr.Number != 1205) {
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

func prepareDirectFixtureWithRetry(ctx context.Context, storage *store.Store, uid uint64, profile, timeProfile string) error {
	return withDeadlockRetry(ctx, func() error {
		return prepareDirectFixture(ctx, storage, uid, profile, timeProfile)
	})
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
	var merged fixtureFile
	for _, path := range []string{firstPath, secondPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var input fixtureFile
		if err := json.Unmarshal(data, &input); err != nil {
			return err
		}
		if input.TimeProfile != "" {
			if merged.TimeProfile != "" && merged.TimeProfile != input.TimeProfile {
				return fmt.Errorf("cannot merge fixture time profiles %q and %q", merged.TimeProfile, input.TimeProfile)
			}
			merged.TimeProfile = input.TimeProfile
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
