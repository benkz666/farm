package gateway

import (
	"sync"

	"farm/server/internal/farm"
)

// RoomHub 按农场主 uid 管理当前房间的连接订阅。
// 广播时在锁外调用推送函数，避免慢连接阻塞订阅表变更。
type RoomHub struct {
	mu    sync.RWMutex
	rooms map[uint64]map[uint64]roomSubscription
}

type roomSubscription struct {
	viewerUID uint64
	push      func(farm.FarmDelta)
}

// NewRoomHub 创建一个可供 Gateway 共享的房间订阅中心。
func NewRoomHub() *RoomHub {
	return &RoomHub{
		rooms: make(map[uint64]map[uint64]roomSubscription),
	}
}

// Subscribe 将 connectionID 的推送函数登记到 ownerUID 对应的农场房间。
// 同一连接重复订阅同一房间时，后一次登记替换前一次。
func (h *RoomHub) Subscribe(ownerUID, connectionID uint64, push func(farm.FarmDelta)) {
	h.SubscribeViewer(ownerUID, connectionID, 0, push)
}

// SubscribeViewer 将观看者身份与连接一起登记，供解除好友关系时撤销订阅。
func (h *RoomHub) SubscribeViewer(ownerUID, connectionID, viewerUID uint64, push func(farm.FarmDelta)) {
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

// Broadcast 向 delta.OwnerUID 房间中当前的全部订阅者推送增量。
func (h *RoomHub) Broadcast(delta farm.FarmDelta) {
	h.BroadcastExcept(delta, 0)
}

// BroadcastExcept 向房间内除 excludedConnectionID 外的订阅者推送增量。
// 写请求的发起方已获得同步 Rsp，跳过它可避免 Delta 抢在该 Rsp 前抵达。
func (h *RoomHub) BroadcastExcept(delta farm.FarmDelta, excludedConnectionID uint64) {
	if h == nil {
		return
	}

	h.mu.RLock()
	subscribers := h.rooms[delta.OwnerUID]
	pushes := make([]func(farm.FarmDelta), 0, len(subscribers))
	for connectionID, subscription := range subscribers {
		if connectionID == excludedConnectionID {
			continue
		}
		pushes = append(pushes, subscription.push)
	}
	h.mu.RUnlock()

	for _, push := range pushes {
		push(copyFarmDelta(delta))
	}
}

func copyFarmDelta(delta farm.FarmDelta) farm.FarmDelta {
	delta.Plots = append([]farm.PlotChange(nil), delta.Plots...)
	return delta
}
