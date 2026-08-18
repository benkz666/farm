package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"farm/server/gateway"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"

	"google.golang.org/grpc"
)

type socialMixedJob struct {
	operation *mixedBehaviorOperation
	account   gatewayAccount
	peer      gatewayAccount
	id        uint64
	enqueued  time.Time
}

// runSocialMixed drives Social's real internal protocol directly. Four public
// Social operations are taken from the shared behavior model; relationship
// checks are derived from EnterFriend and all cross-farm operations.
func runSocialMixed(
	ctx context.Context,
	conn grpc.ClientConnInterface,
	fixturePath, modelPath string,
	qps int,
	duration time.Duration,
	concurrency, hotUsers int,
	measurementStartUnixMS int64,
) (result, error) {
	fixtureData, err := os.ReadFile(fixturePath)
	if err != nil {
		return result{}, err
	}
	var fixture struct {
		Accounts []gatewayAccount `json:"accounts"`
	}
	if err := json.Unmarshal(fixtureData, &fixture); err != nil {
		return result{}, fmt.Errorf("decode Social fixture: %w", err)
	}
	if len(fixture.Accounts) < 10 {
		return result{}, fmt.Errorf("social-mixed needs at least 10 accounts")
	}
	if hotUsers <= 0 || hotUsers >= len(fixture.Accounts) {
		return result{}, fmt.Errorf("social hot users %d must be within fixture size %d", hotUsers, len(fixture.Accounts))
	}

	modelData, err := os.ReadFile(modelPath)
	if err != nil {
		return result{}, err
	}
	var model mixedBehaviorModel
	if err := json.Unmarshal(modelData, &model); err != nil {
		return result{}, fmt.Errorf("decode behavior model: %w", err)
	}
	direct := map[string]bool{
		"friend-list": true, "gen-share": true,
		"search-user": true, "list-friend-requests": true,
	}
	relation := map[string]bool{
		"enter-friend": true, "water-cross": true, "weed-cross": true,
		"pest-cross": true, "steal": true,
	}
	operations := make([]*mixedBehaviorOperation, 0, 5)
	var relationQPS, referenceTotal float64
	for index := range model.Operations {
		op := model.Operations[index]
		if !op.Enabled || op.ReferenceQPS <= 0 {
			continue
		}
		if relation[op.Name] {
			relationQPS += op.ReferenceQPS
		}
		if direct[op.Name] {
			copyOfOperation := op
			operations = append(operations, &copyOfOperation)
			referenceTotal += op.ReferenceQPS
		}
	}
	if relationQPS <= 0 || len(operations) != 4 {
		return result{}, fmt.Errorf("behavior model lacks complete Social operations")
	}
	operations = append(operations, &mixedBehaviorOperation{
		Name: "are-friends", ReferenceQPS: relationQPS, Enabled: true,
	})
	referenceTotal += relationQPS

	client := farmv1.NewSocialServiceClient(conn)
	if err := prewarmSocialHotSet(ctx, client, fixture.Accounts[:hotUsers], concurrency); err != nil {
		return result{}, err
	}
	stateReadyAt := time.Now()

	jobs := make(chan socialMixedJob, concurrency)
	recorded := &recorder{}
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				code, callErr := executeSocialMixedJob(ctx, client, job)
				ok := callErr == nil && code == 0
				job.operation.recorder.add(time.Since(job.enqueued), ok, code)
				recorded.add(time.Since(job.enqueued), ok, code)
			}
		}()
	}
	if measurementStartUnixMS > 0 {
		if err := waitUntil(ctx, time.UnixMilli(measurementStartUnixMS)); err != nil {
			close(jobs)
			workers.Wait()
			return result{}, err
		}
	}
	started := time.Now()
	if measurementStartUnixMS > 0 {
		started = time.UnixMilli(measurementStartUnixMS)
	}
	planned := oneShotOperationCount(qps, duration)
	coldCount := len(fixture.Accounts) - hotUsers
	var scheduled uint64
	for scheduled < uint64(planned) && ctx.Err() == nil {
		if err := waitUntil(ctx, started.Add(time.Duration(scheduled)*time.Second/time.Duration(qps))); err != nil {
			break
		}
		operation := selectMixedOperation(operations, referenceTotal, scheduled)
		// Exactly one in five arrivals is cold; the remaining four reuse a
		// bounded hot set. This makes the declared 80/20 split reproducible.
		var accountIndex int
		if scheduled%5 == 0 {
			accountIndex = hotUsers + int((scheduled/5)%uint64(coldCount))
		} else {
			accountIndex = int(scheduled % uint64(hotUsers))
		}
		peerIndex := (accountIndex + 1) % len(fixture.Accounts)
		job := socialMixedJob{
			operation: operation, account: fixture.Accounts[accountIndex],
			peer: fixture.Accounts[peerIndex], id: scheduled + 1, enqueued: time.Now(),
		}
		select {
		case jobs <- job:
		default:
			operation.recorder.add(0, false, -2)
			recorded.add(0, false, -2)
		}
		scheduled++
	}
	close(jobs)
	workers.Wait()
	wall := time.Since(started)
	measured := summarize("social-mixed-"+model.Name, qps, scheduled, wall, recorded, ctx.Err() != nil)
	measured.StartedMS = started.UnixMilli()
	measured.EndedMS = started.Add(wall).UnixMilli()
	measured.StateReadyMS = stateReadyAt.UnixMilli()
	measured.StateWindowMillis = max(started.Sub(stateReadyAt).Milliseconds(), 0)
	measured.MeasurementMillis = duration.Milliseconds()
	measured.DrainMillis = max((wall - duration).Milliseconds(), 0)
	measured.CompletionQPS = measured.ActualQPS
	measured.ActualQPS = float64(measured.Succeeded) / duration.Seconds()
	measured.Steps = make(map[string]stepResult, len(operations))
	for _, operation := range operations {
		target := float64(qps) * operation.ReferenceQPS / referenceTotal
		measured.Steps[operation.Name] = summarizeWeightedStep(&operation.recorder, target, duration)
	}
	return measured, nil
}

func executeSocialMixedJob(ctx context.Context, client farmv1.SocialServiceClient, job socialMixedJob) (int32, error) {
	uid, err := strconv.ParseUint(job.account.UID, 10, 64)
	if err != nil || uid == 0 {
		return -1, fmt.Errorf("invalid uid %q", job.account.UID)
	}
	peerUID, _ := strconv.ParseUint(job.peer.UID, 10, 64)
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if job.operation.Name == "are-friends" {
		_, err := client.AreFriends(callCtx, &farmv1.AreFriendsRequest{Uid: uid, PeerUid: peerUID})
		return 0, err
	}
	request := clientwire.Envelope{ClientSeq: uint32(job.id)}
	switch job.operation.Name {
	case "friend-list":
		request.Cmd, request.Payload = gateway.CommandFriendList, json.RawMessage(`{}`)
	case "gen-share":
		request.Cmd, request.Payload = gateway.CommandGenShareLink, json.RawMessage(`{}`)
	case "search-user":
		username := job.account.PeerUsername
		if username == "" {
			username = job.peer.Username
		}
		request.Cmd = gateway.CommandSearchUser
		request.Payload = json.RawMessage(fmt.Sprintf(`{"username":%q}`, username))
	case "list-friend-requests":
		request.Cmd, request.Payload = gateway.CommandListFriendRequests, json.RawMessage(`{}`)
	default:
		return -1, fmt.Errorf("unsupported Social operation %q", job.operation.Name)
	}
	wire, err := clientwire.EnvelopeToProto(request)
	if err != nil {
		return -1, err
	}
	response, err := client.ExecuteClientCommand(callCtx, &farmv1.ClientCommandRequest{
		Uid: uid, RouteUid: uid, Envelope: wire, PreferPrepared: true,
	})
	if err != nil || response == nil || response.Envelope == nil {
		return -1, err
	}
	return response.Envelope.Err, nil
}

func prewarmSocialHotSet(ctx context.Context, client farmv1.SocialServiceClient, accounts []gatewayAccount, concurrency int) error {
	concurrency = max(1, min(concurrency, 256))
	jobs := make(chan int, concurrency)
	errs := make(chan error, 1)
	var wait sync.WaitGroup
	for range concurrency {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				uid, err := strconv.ParseUint(accounts[index].UID, 10, 64)
				if err != nil || uid == 0 {
					continue
				}
				peer := accounts[(index+1)%len(accounts)]
				peerUID, _ := strconv.ParseUint(peer.UID, 10, 64)
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err = client.AreFriends(callCtx, &farmv1.AreFriendsRequest{Uid: uid, PeerUid: peerUID})
				cancel()
				if err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}()
	}
	for index := range accounts {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	select {
	case err := <-errs:
		return fmt.Errorf("prewarm Social hot set: %w", err)
	default:
		return nil
	}
}
