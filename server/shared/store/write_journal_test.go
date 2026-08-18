package store

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/outbox"
	"github.com/redis/go-redis/v9"
)

func TestWriteJournalRecordKeepsUIDMetadataAsDecimalStrings(t *testing.T) {
	uid := uint64(math.MaxUint64 - 1)
	record := writeJournalRecord{
		Version: writeJournalVersion,
		EventID: "farm_commit:test",
		Kind:    writeJournalFarmCommit,
		UID:     "18446744073709551614",
		FarmSeq: "9007199254740993",
		Commit: &outbox.FarmCommit{Snapshot: &farm.Aggregate{
			UID: uid, FarmSeq: 9_007_199_254_740_993,
		}},
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeWriteJournalRecord(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.UID != record.UID || decoded.FarmSeq != record.FarmSeq || decoded.Commit.Snapshot.UID != uid {
		t.Fatalf("decoded record = %#v", decoded)
	}
}

func TestCoalesceIncrementalMutationsKeepsLatestExactRows(t *testing.T) {
	records := []writeJournalRecord{
		{EventID: "first", Commit: &outbox.FarmCommit{Mutation: &farmv1.FarmWriteMutation{
			Uid: 42, FarmSeq: 1, PlayerMask: outbox.PlayerEconomy,
			Items: []*farmv1.FarmWriteItem{{Key: "fruit:1", Count: 5}},
			Plots: []*farmv1.FarmWritePlot{{Index: 1, State: 2}},
		}}},
		{EventID: "second", Commit: &outbox.FarmCommit{Mutation: &farmv1.FarmWriteMutation{
			Uid: 42, FarmSeq: 2, PlayerMask: outbox.PlayerPet,
			Items: []*farmv1.FarmWriteItem{{Key: "fruit:1", Count: 4}, {Key: "seed:1", Count: 3}},
			Plots: []*farmv1.FarmWritePlot{{Index: 2, State: 3}},
		}}},
	}
	commits, latest := coalesceJournalFarmCommits(records)
	if len(commits) != 1 || latest[42] != "second" {
		t.Fatalf("coalesced=%#v latest=%#v", commits, latest)
	}
	mutation := commits[0].Mutation
	if mutation.FarmSeq != 2 || mutation.PlayerMask != outbox.PlayerEconomy|outbox.PlayerPet || len(mutation.Plots) != 2 || len(mutation.Items) != 2 {
		t.Fatalf("mutation = %#v", mutation)
	}
	if mutation.Items[0].Key != "fruit:1" || mutation.Items[0].Count != 4 {
		t.Fatalf("latest item was not retained: %#v", mutation.Items)
	}
}

func TestAdaptiveProjectionLimiterHonorsReducedLimit(t *testing.T) {
	limiter := newAdaptiveProjectionLimiter(4)
	limiter.SetLimit(1)
	if !limiter.Acquire(context.Background()) {
		t.Fatal("first acquire failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if limiter.Acquire(ctx) {
		t.Fatal("second acquire bypassed reduced limit")
	}
	limiter.Release()
	if !limiter.Acquire(context.Background()) {
		t.Fatal("acquire after release failed")
	}
	limiter.Release()
}

func TestJournalProtoBufferPoolReturnsEmptyReusableBuffer(t *testing.T) {
	buffer := acquireJournalProtoBuffer()
	*buffer = append(*buffer, 1, 2, 3)
	releaseJournalProtoBuffer(buffer)

	reused := acquireJournalProtoBuffer()
	defer releaseJournalProtoBuffer(reused)
	if len(*reused) != 0 {
		t.Fatalf("pooled protobuf buffer length = %d, want 0", len(*reused))
	}
}

func TestProjectionLimiterPrioritizesForegroundAndAvoidsBatchGapOscillation(t *testing.T) {
	journal := NewFarmWriteJournal(nil, nil, FarmWriteJournalConfig{Projectors: 4})
	journal.foregroundQueue.Store(1)
	journal.adjustProjectionLimit()
	journal.projectLimiter.mu.Lock()
	limit := journal.projectLimiter.limit
	journal.projectLimiter.mu.Unlock()
	if limit != 1 {
		t.Fatalf("projector limit under foreground pressure = %d, want 1", limit)
	}

	journal.foregroundQueue.Store(0)
	journal.adjustProjectionLimit()
	journal.projectLimiter.mu.Lock()
	limit = journal.projectLimiter.limit
	journal.projectLimiter.mu.Unlock()
	if limit != 1 {
		t.Fatalf("projector limit in foreground hold window = %d, want 1", limit)
	}

	journal.lastForeground.Store(time.Now().Add(-projectionForegroundHold - time.Millisecond).UnixNano())
	journal.adjustProjectionLimit()
	journal.projectLimiter.mu.Lock()
	limit = journal.projectLimiter.limit
	journal.projectLimiter.mu.Unlock()
	if limit != 4 {
		t.Fatalf("projector limit after foreground hold = %d, want 4", limit)
	}
}

func TestProjectionLimiterAddsOnlyOneWorkerForConfirmedBacklog(t *testing.T) {
	journal := NewFarmWriteJournal(nil, nil, FarmWriteJournalConfig{Projectors: 4, BatchSize: 8})
	journal.foregroundQueue.Store(1)
	journal.markProjectionBacklog(8)
	journal.adjustProjectionLimit()
	journal.projectLimiter.mu.Lock()
	limit := journal.projectLimiter.limit
	journal.projectLimiter.mu.Unlock()
	if limit != 2 {
		t.Fatalf("projector limit with foreground and full batch = %d, want 2", limit)
	}

	journal.projectionBacklog.Store(0)
	journal.adjustProjectionLimit()
	journal.projectLimiter.mu.Lock()
	limit = journal.projectLimiter.limit
	journal.projectLimiter.mu.Unlock()
	if limit != 1 {
		t.Fatalf("projector limit after backlog pressure expires = %d, want 1", limit)
	}
}

func TestProjectionLimiterScalesFromAggregateBacklog(t *testing.T) {
	journal := NewFarmWriteJournal(nil, nil, FarmWriteJournalConfig{Projectors: 8, BatchSize: 8})
	journal.foregroundQueue.Store(1)

	journal.projectionBacklog.Store(8 * projectionMediumBatches)
	journal.adjustProjectionLimit()
	if limit := journal.projectLimiter.Limit(); limit != 4 {
		t.Fatalf("projector limit at medium aggregate backlog = %d, want 4", limit)
	}

	journal.projectionBacklog.Store(8 * projectionHighBatches)
	journal.adjustProjectionLimit()
	if limit := journal.projectLimiter.Limit(); limit != 8 {
		t.Fatalf("projector limit at high aggregate backlog = %d, want 8", limit)
	}
}

func TestProjectionLimiterPrioritizesForegroundBarrier(t *testing.T) {
	journal := NewFarmWriteJournal(nil, nil, FarmWriteJournalConfig{Projectors: 4, BatchSize: 8})
	journal.foregroundQueue.Store(1)
	journal.barrierWaiters.Store(1)
	journal.adjustProjectionLimit()
	if limit := journal.projectLimiter.Limit(); limit != 4 {
		t.Fatalf("projector limit while a foreground barrier waits = %d, want 4", limit)
	}

	journal.barrierWaiters.Store(0)
	journal.adjustProjectionLimit()
	if limit := journal.projectLimiter.Limit(); limit != 1 {
		t.Fatalf("projector limit after foreground barrier drains = %d, want 1", limit)
	}
}

func TestCoalesceJournalFarmCommitsUsesLatestSnapshotAndSmallestSafePlan(t *testing.T) {
	records := []writeJournalRecord{
		{
			EventID: "first",
			Commit: &outbox.FarmCommit{
				Snapshot: &farm.Aggregate{UID: 42, FarmSeq: 10},
				Plan:     outbox.PersistPlan{Mode: outbox.PersistPlot, PlotIndex: 1},
			},
		},
		{
			EventID: "second",
			Commit: &outbox.FarmCommit{
				Snapshot: &farm.Aggregate{UID: 42, FarmSeq: 11},
				Plan: outbox.PersistPlan{
					Mode: outbox.PersistPlot, PlotIndex: 1, IncludeItems: true,
				},
			},
		},
	}
	commits, latest := coalesceJournalFarmCommits(records)
	if len(commits) != 1 || commits[0].Snapshot.FarmSeq != 11 {
		t.Fatalf("commits = %#v", commits)
	}
	if commits[0].Plan.Mode != outbox.PersistPlot || commits[0].Plan.PlotIndex != 1 || !commits[0].Plan.IncludeItems {
		t.Fatalf("merged plan = %#v", commits[0].Plan)
	}
	if latest[42] != "second" {
		t.Fatalf("latest ID = %q", latest[42])
	}
}

func TestCoalesceJournalFarmCommitsPromotesMixedWritesToFullSnapshot(t *testing.T) {
	records := []writeJournalRecord{
		{EventID: "plot", Commit: &outbox.FarmCommit{
			Snapshot: &farm.Aggregate{UID: 7, FarmSeq: 1},
			Plan:     outbox.PersistPlan{Mode: outbox.PersistPlot, PlotIndex: 0},
		}},
		{EventID: "economy", Commit: &outbox.FarmCommit{
			Snapshot: &farm.Aggregate{UID: 7, FarmSeq: 2},
			Plan:     outbox.PersistPlan{Mode: outbox.PersistEconomy},
		}},
	}
	commits, _ := coalesceJournalFarmCommits(records)
	if len(commits) != 1 || commits[0].Plan.Mode != outbox.PersistFull {
		t.Fatalf("commits = %#v", commits)
	}
}

func TestJournalStreamAndLatestKeysShareRedisClusterSlot(t *testing.T) {
	journal := NewFarmWriteJournal(nil, nil, FarmWriteJournalConfig{
		InstanceID: "farm/{unsafe}", Shards: 64,
	})
	shard := journal.shard(12345)
	stream := journal.streamKey(shard)
	latest := journal.latestKey(shard, 12345)
	pending := journal.pendingUIDKey(shard, 12345)
	tag := "{" + journal.streamTag(shard) + "}"
	if !contains(stream, tag) || !contains(latest, tag) || !contains(pending, tag) {
		t.Fatalf("keys do not share hash tag: %q, %q, %q", stream, latest, pending)
	}
}

func TestNormalizeWriteJournalProjectorsDoesNotExceedShards(t *testing.T) {
	config := normalizeFarmWriteJournalConfig(FarmWriteJournalConfig{
		InstanceID: "farm-0", Shards: 2, Projectors: 8,
	})
	if config.Projectors != 2 {
		t.Fatalf("projectors = %d, want 2", config.Projectors)
	}
}

func TestProjectionGroupsMovesACKsBehindStateAndJoinsFarmMutations(t *testing.T) {
	kinds := []string{
		writeJournalFarmCommit,
		writeJournalOutboxAck,
		writeJournalFarmCommit,
		writeJournalTaskAdvance,
		writeJournalOutboxAck,
		writeJournalFarmCommit,
	}
	messages := make([]redis.XMessage, len(kinds))
	records := make([]writeJournalRecord, len(kinds))
	for index, kind := range kinds {
		messages[index] = redis.XMessage{ID: string(rune('a' + index))}
		records[index] = writeJournalRecord{Kind: kind}
	}

	groups := projectionGroups(messages, records)
	if len(groups) != 4 {
		t.Fatalf("projection groups = %d, want 4", len(groups))
	}
	wantKinds := []string{
		writeJournalFarmCommit, writeJournalTaskAdvance,
		writeJournalFarmCommit, writeJournalOutboxAck,
	}
	wantSizes := []int{2, 1, 1, 2}
	for index := range wantKinds {
		if groups[index].kind != wantKinds[index] || len(groups[index].records) != wantSizes[index] {
			t.Fatalf("group %d = %q/%d, want %q/%d",
				index, groups[index].kind, len(groups[index].records), wantKinds[index], wantSizes[index])
		}
	}
	if groups[0].messages[0].ID != "a" || groups[0].messages[1].ID != "c" {
		t.Fatalf("joined Farm message order = %#v", groups[0].messages)
	}
	if groups[3].messages[0].ID != "b" || groups[3].messages[1].ID != "e" {
		t.Fatalf("batched ACK message order = %#v", groups[3].messages)
	}
}

func TestOwnerUIDFromOutboxEventID(t *testing.T) {
	uid, err := ownerUIDFromOutboxEventID("cross_result:42:99:123")
	if err != nil || uid != 42 {
		t.Fatalf("uid=%d err=%v", uid, err)
	}
	if _, err := ownerUIDFromOutboxEventID("bad:42"); err == nil {
		t.Fatal("invalid event ID was accepted")
	}
}

func TestJournalStreamIDOrdering(t *testing.T) {
	ms, sequence, err := parseJournalStreamID("1844674407370-42")
	if err != nil || ms != 1_844_674_407_370 || sequence != 42 {
		t.Fatalf("stream id parsed as %d-%d err=%v", ms, sequence, err)
	}
	if !journalStreamAfter(ms, 43, ms, 42) || !journalStreamAfter(ms+1, 0, ms, 42) {
		t.Fatal("new stream position was not ordered after the high-water mark")
	}
	if journalStreamAfter(ms, 42, ms, 42) || journalStreamAfter(ms-1, 999, ms, 42) {
		t.Fatal("replayed stream position passed the high-water mark")
	}
	if _, _, err := parseJournalStreamID("bad"); err == nil {
		t.Fatal("invalid stream id was accepted")
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
