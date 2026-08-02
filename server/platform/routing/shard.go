// Package routing 提供 uid 到 FarmServer 的逻辑分片与路由。
//
// 期 4 多 Farm 拓扑（规格 2026-07-27-phase4-social-loop.md §3）：
//   - 1024 个逻辑片，LogicalShard(uid)=hash(uid)%1024
//   - RouteTable 把逻辑片区间映射到 farm 实例 ID
//   - Gateway 无状态，按 RouteTable 转发；Farm 只载本实例逻辑片
//
// 哈希选用 FNV-1a 64-bit：稳定、无外部依赖、分布足够均匀；
// 不用 crypto hash 是因为这里不需要抗碰撞，只要可复现。
package routing

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
)

// LogicalShardCount 是协议钉死的逻辑分片总数（规格 §3 拓扑）。
// 改动它会让历史 uid 路由错位，禁止在期 4 调整。
const LogicalShardCount = 1024

// LogicalShard 返回 uid 所属逻辑分片，结果在 [0, LogicalShardCount)。
func LogicalShard(uid uint64) int {
	h := fnv.New64a()
	// 直接写 8 字节小端序，避免 binary.Write 的反射开销且跨平台确定。
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(uid >> (8 * i))
	}
	h.Write(b[:])
	return int(h.Sum64() % LogicalShardCount)
}

// RouteEntry 是一段连续逻辑片（闭区间）到 farm 实例的映射。
type RouteEntry struct {
	ShardStart int    `json:"shard_start"`
	ShardEnd   int    `json:"shard_end"`
	FarmID     string `json:"farm_id"`
}

// RouteTableFile 是 route-table.example.json 的磁盘结构。
type RouteTableFile struct {
	LogicalShards int          `json:"logical_shards"`
	Routes        []RouteEntry `json:"routes"`
}

// RouteTable 把 uid 解析到 farm 实例 ID。构造后只读，可并发查询。
type RouteTable struct {
	logicalShards int
	entries       []RouteEntry
}

// LoadRouteTable 从 JSON 文件加载路由表。
func LoadRouteTable(path string) (*RouteTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("routing: read route table: %w", err)
	}
	return ParseRouteTable(data)
}

// ParseRouteTable 从 JSON 字节解析路由表，并校验区间合法、不重叠、完整覆盖。
func ParseRouteTable(data []byte) (*RouteTable, error) {
	var f RouteTableFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("routing: parse route table: %w", err)
	}
	if f.LogicalShards != LogicalShardCount {
		return nil, fmt.Errorf("routing: logical_shards=%d 与协议 %d 不一致", f.LogicalShards, LogicalShardCount)
	}
	if len(f.Routes) == 0 {
		return nil, fmt.Errorf("routing: routes 为空")
	}
	covered := make([]bool, f.LogicalShards)
	for _, e := range f.Routes {
		if e.ShardStart < 0 || e.ShardEnd >= f.LogicalShards || e.ShardStart > e.ShardEnd {
			return nil, fmt.Errorf("routing: 非法区间 [%d,%d]", e.ShardStart, e.ShardEnd)
		}
		if e.FarmID == "" {
			return nil, fmt.Errorf("routing: 区间 [%d,%d] farm_id 为空", e.ShardStart, e.ShardEnd)
		}
		for s := e.ShardStart; s <= e.ShardEnd; s++ {
			if covered[s] {
				return nil, fmt.Errorf("routing: 逻辑片 %d 被多个区间覆盖", s)
			}
			covered[s] = true
		}
	}
	for s := 0; s < f.LogicalShards; s++ {
		if !covered[s] {
			return nil, fmt.Errorf("routing: 逻辑片 %d 未被任何区间覆盖", s)
		}
	}
	return &RouteTable{logicalShards: f.LogicalShards, entries: f.Routes}, nil
}

// FarmID 返回 uid 所属 farm 实例 ID。
func (r *RouteTable) FarmID(uid uint64) (string, error) {
	shard := LogicalShard(uid)
	for _, e := range r.entries {
		if shard >= e.ShardStart && shard <= e.ShardEnd {
			return e.FarmID, nil
		}
	}
	return "", fmt.Errorf("routing: 逻辑片 %d 未命中任何路由", shard)
}
