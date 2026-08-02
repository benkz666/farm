package routing

import (
	"path/filepath"
	"testing"
)

func TestLogicalShardStable(t *testing.T) {
	seen := make(map[int]int)
	for uid := uint64(0); uid < 5000; uid++ {
		s := LogicalShard(uid)
		if s < 0 || s >= LogicalShardCount {
			t.Fatalf("uid=%d shard=%d 越界", uid, s)
		}
		if LogicalShard(uid) != s {
			t.Fatalf("uid=%d 不稳定", uid)
		}
		seen[s]++
	}
	if len(seen) < LogicalShardCount/2 {
		t.Fatalf("分布过窄，仅命中 %d 个分片", len(seen))
	}
}

func TestRouteTableExample(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "route-table.example.json")
	rt, err := LoadRouteTable(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var uidLow, uidHigh uint64
	var foundLow, foundHigh bool
	for uid := uint64(0); uid < 100_000; uid++ {
		s := LogicalShard(uid)
		if !foundLow && s <= 511 {
			uidLow = uid
			foundLow = true
		}
		if !foundHigh && s >= 512 {
			uidHigh = uid
			foundHigh = true
		}
		if foundLow && foundHigh {
			break
		}
	}
	if !foundLow || !foundHigh {
		t.Fatalf("未找到合适 uid: low=%v high=%v", uidLow, uidHigh)
	}
	if got, err := rt.FarmID(uidLow); err != nil || got != "farm-0" {
		t.Fatalf("uidLow=%d want farm-0 got %q err=%v", uidLow, got, err)
	}
	if got, err := rt.FarmID(uidHigh); err != nil || got != "farm-1" {
		t.Fatalf("uidHigh=%d want farm-1 got %q err=%v", uidHigh, got, err)
	}
}

func TestParseRouteTableRejectsGap(t *testing.T) {
	bad := []byte(`{"logical_shards":1024,"routes":[{"shard_start":0,"shard_end":510,"farm_id":"farm-0"},{"shard_start":512,"shard_end":1023,"farm_id":"farm-1"}]}`)
	if _, err := ParseRouteTable(bad); err == nil {
		t.Fatal("gap 未被拒绝")
	}
}

func TestParseRouteTableRejectsOverlap(t *testing.T) {
	bad := []byte(`{"logical_shards":1024,"routes":[{"shard_start":0,"shard_end":512,"farm_id":"farm-0"},{"shard_start":512,"shard_end":1023,"farm_id":"farm-1"}]}`)
	if _, err := ParseRouteTable(bad); err == nil {
		t.Fatal("overlap 未被拒绝")
	}
}

func TestParseRouteTableRejectsBadShardCount(t *testing.T) {
	bad := []byte(`{"logical_shards":512,"routes":[{"shard_start":0,"shard_end":511,"farm_id":"farm-0"}]}`)
	if _, err := ParseRouteTable(bad); err == nil {
		t.Fatal("bad shard count 未被拒绝")
	}
}

func TestParseRouteTableRejectsEmptyFarmID(t *testing.T) {
	bad := []byte(`{"logical_shards":1024,"routes":[{"shard_start":0,"shard_end":1023,"farm_id":""}]}`)
	if _, err := ParseRouteTable(bad); err == nil {
		t.Fatal("empty farm_id 未被拒绝")
	}
}
