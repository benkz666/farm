package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"farm/server/internal/connreg"
	"farm/server/internal/farm"
	"farm/server/internal/obs"
	"farm/server/internal/store"
	"farm/server/internal/wireenv"
)

// RoomHub 按农场主 uid 管理当前房间的连接订阅。
// 广播时在锁外调用推送函数，避免慢连接阻塞订阅表变更。
type RoomHub struct {
	mu              sync.RWMutex
	rooms           map[uint64]map[uint64]roomSubscription
	encodeFarmDelta func(farm.FarmDelta) ([]byte, error)
	metrics         *obs.Metrics
}

type roomSubscription struct {
	viewerUID uint64
	push      func(delta farm.FarmDelta, encoded []byte)
}

// NewRoomHub 创建一个可供 Gateway 共享的房间订阅中心。
func NewRoomHub() *RoomHub {
	return &RoomHub{
		rooms:           make(map[uint64]map[uint64]roomSubscription),
		encodeFarmDelta: wireenv.EncodeFarmDelta,
	}
}

// Subscribe 将 connectionID 的推送函数登记到 ownerUID 对应的农场房间。
// 同一连接重复订阅同一房间时，后一次登记替换前一次。
func (h *RoomHub) Subscribe(ownerUID, connectionID uint64, push func(farm.FarmDelta, []byte)) {
	h.SubscribeViewer(ownerUID, connectionID, 0, push)
}

// SubscribeViewer 将观看者身份与连接一起登记，供解除好友关系时撤销订阅。
func (h *RoomHub) SubscribeViewer(ownerUID, connectionID, viewerUID uint64, push func(farm.FarmDelta, []byte)) {
	if h == nil || push == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms == nil {
		h.rooms = make(map[uint64]map[uint64]roomSubscription)
	}
	if h.rooms[ownerUID] == nil {
		h.rooms[ownerUID] = make(map[uint64]roomSubscription)
	}
	h.rooms[ownerUID][connectionID] = roomSubscription{viewerUID: viewerUID, push: push}
}

// Unsubscribe 移除 connectionID 对 ownerUID 房间的订阅。
func (h *RoomHub) Unsubscribe(ownerUID, connectionID uint64) {
	if h == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	subscribers := h.rooms[ownerUID]
	if subscribers == nil {
		return
	}
	delete(subscribers, connectionID)
	if len(subscribers) == 0 {
		delete(h.rooms, ownerUID)
	}
}

// RevokeViewer 移除 viewerUID 在 ownerUID 农场的全部订阅连接。
func (h *RoomHub) RevokeViewer(ownerUID, viewerUID uint64) {
	if h == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	subscribers := h.rooms[ownerUID]
	for connectionID, subscription := range subscribers {
		if subscription.viewerUID == viewerUID {
			delete(subscribers, connectionID)
		}
	}
	if len(subscribers) == 0 {
		delete(h.rooms, ownerUID)
	}
}

// Publish broadcasts a cross-farm owner commit to every local room viewer.
// It implements cross.DeltaPublisher for the single-process development role.
func (g *Gateway) Publish(_ context.Context, delta farm.FarmDelta, _ connreg.ConnRef) error {
	if g == nil || g.rooms == nil {
		return errors.New("gateway: room hub is not configured")
	}
	g.rooms.Broadcast(delta)
	return nil
}

// PublishPlayerDelta sends a personal state snapshot to every local connection
// authenticated as uid. It implements cross.PlayerDeltaPublisher in all-in-one
// development mode; multi-process Farm servers use farmrpc fan-out instead.
func (g *Gateway) PublishPlayerDelta(_ context.Context, uid uint64, delta farm.PlayerDelta) error {
	if g == nil || uid == 0 {
		return errors.New("gateway: invalid PlayerDelta target")
	}
	g.connections.Range(func(_, value any) bool {
		connection, ok := value.(*wsConnection)
		if ok && connection.uid == uid && connection.authed {
			connection.pushPlayerDelta(delta)
		}
		return true
	})
	return nil
}

// PublishTaskNotify sends an authoritative daily-task snapshot to every local
// connection authenticated as uid. Each connection retains its own latest
// snapshot per task until its mailbox can write it.
func (g *Gateway) PublishTaskNotify(_ context.Context, uid uint64, task store.Task) error {
	if g == nil || uid == 0 {
		return errors.New("gateway: invalid TaskNotify target")
	}
	g.connections.Range(func(_, value any) bool {
		connection, ok := value.(*wsConnection)
		if ok && connection.uid == uid && connection.authed {
			connection.enqueueTaskNotify(task)
		}
		return true
	})
	return nil
}

// PublishMailNotify 向 uid 的本机在线连接投递 MailNotify（9004）。每条连接都有独立
// 有界 mailbox，因此单个慢客户端不会阻塞同玩家的其他会话。
func (g *Gateway) PublishMailNotify(_ context.Context, uid uint64, kind string) error {
	if g == nil || uid == 0 {
		return errors.New("gateway: invalid MailNotify target")
	}
	g.connections.Range(func(_, value any) bool {
		connection, ok := value.(*wsConnection)
		if ok && connection.uid == uid && connection.authed {
			connection.enqueueMailNotify(kind)
		}
		return true
	})
	return nil
}

// pushMailNotify 保留好友写路径的 best-effort 语义。
func (g *Gateway) pushMailNotify(uid uint64, kind string) {
	_ = g.PublishMailNotify(context.Background(), uid, kind)
}

// Broadcast 向 delta.OwnerUID 房间中当前的全部订阅者推送增量。
func (h *RoomHub) Broadcast(delta farm.FarmDelta) {
	h.BroadcastExcept(delta, 0)
}

// BroadcastExcept 向房间内除 excludedConnectionID 外的订阅者推送增量。
// 写请求的发起方已获得同步 Rsp，跳过它可避免 Delta 抢在该 Rsp 前抵达。
// Envelope 只编码一次；各连接复用同一 []byte（正常路径禁止逐连接 marshal）。
func (h *RoomHub) BroadcastExcept(delta farm.FarmDelta, excludedConnectionID uint64) {
	if h == nil {
		return
	}

	encode := h.encodeFarmDelta
	if encode == nil {
		encode = wireenv.EncodeFarmDelta
	}
	encodeStart := time.Now()
	encoded, err := encode(delta)
	encodeDur := time.Since(encodeStart)
	if err != nil {
		return
	}

	h.mu.RLock()
	subscribers := h.rooms[delta.OwnerUID]
	pushes := make([]func(farm.FarmDelta, []byte), 0, len(subscribers))
	for connectionID, subscription := range subscribers {
		if connectionID == excludedConnectionID {
			continue
		}
		pushes = append(pushes, subscription.push)
	}
	h.mu.RUnlock()

	copied := copyFarmDelta(delta)
	pushStart := time.Now()
	for _, push := range pushes {
		push(copied, encoded)
	}
	if m := h.metrics; m != nil && len(pushes) > 0 {
		// 本地房间一次广播 = 1 个 batch（无跨 Gateway 拆分）。
		m.ObserveDeltaBroadcast(1, len(pushes), encodeDur, time.Since(pushStart))
	}
}

func copyFarmDelta(delta farm.FarmDelta) farm.FarmDelta {
	delta.Plots = append([]farm.PlotChange(nil), delta.Plots...)
	return delta
}
