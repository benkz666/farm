// Package crossfarm models the asynchronous visitor-side half of a cross-farm action.
package crossfarm

import (
	"time"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	"farm/server/shared/errcode"
)

// ActionKind identifies the owner-farm operation requested by a visitor.
// Values cross protobuf/outbox boundaries and must remain stable.
type ActionKind string

const (
	Water      ActionKind = "water"
	RemoveWeed ActionKind = "remove_weed"
	RemovePest ActionKind = "remove_pest"
	Steal      ActionKind = "steal"
)

// CrossAction is published after the visitor has reserved its local resources.
type CrossAction struct {
	ReqID        uint64     `json:"req_id"`
	Kind         ActionKind `json:"kind"`
	VisitorUID   uint64     `json:"visitor_uid"`
	OwnerUID     uint64     `json:"owner_uid"`
	PlotIndex    uint8      `json:"plot_index"`
	CropID       uint16     `json:"crop_id,omitempty"`
	Compensation int64      `json:"compensation,omitempty"`
	// FriendshipVerified means the trusted Gateway has already performed the
	// authoritative friendship check for this internal request.
	FriendshipVerified bool `json:"friendship_verified,omitempty"`
	// Originator is transport-only metadata for the colocated fast path. It is
	// deliberately not persisted in visitor reservations or event payloads.
	Originator presence.ConnRef `json:"-"`
}

// CrossResult is sent back to the visitor after the owner farm decides an action.
// StealIntercepted is the sole non-OK code that settles (rather than releases)
// the visitor's frozen compensation.
type CrossResult struct {
	ReqID        uint64       `json:"req_id"`
	VisitorUID   uint64       `json:"visitor_uid"`
	OwnerUID     uint64       `json:"owner_uid"`
	Code         errcode.Code `json:"code"`
	CropID       uint16       `json:"crop_id,omitempty"`
	Amount       uint16       `json:"amount,omitempty"`
	Compensation int64        `json:"compensation,omitempty"`
	DogType      farm.DogType `json:"dog_type,omitempty"`
}

// CrossExecution is the result of the colocated one-RPC fast path. Production
// runtimes combine both UID mutations into one durable journal command; the
// distributed fallback continues to use the three-boundary Saga.
type CrossExecution struct {
	Result         CrossResult
	Reward         VisitorReward
	PlayerDelta    *farm.PlayerDelta
	FarmDelta      *farm.FarmDelta
	Code           errcode.Code
	OwnerCommitted bool
	AckRequired    bool
}

// PendingTimeout 是 Gateway 等待主人侧回执后应答客户端的时限。
//
// 到点只应答客户端，不回滚访客预占——预占的回滚责任在访客聚合里，由
// farm.CrossPendingTimeout 兜底。这样 5 到 10 秒之间迟到的回执依然能正确结算，
// 而不是先退款再收到「主人侧其实已经扣了果实」。
const PendingTimeout = 5 * time.Second
