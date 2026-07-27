// Package cross models the asynchronous visitor-side half of a cross-farm action.
package cross

import "farm/server/internal/pkgerr"

// ActionKind identifies the owner-farm operation requested by a visitor.
// Values are serialized on the event bus and must remain stable.
type ActionKind string

const (
	Water      ActionKind = "water"
	RemoveWeed ActionKind = "remove_weed"
	RemovePest ActionKind = "remove_pest"
	Steal      ActionKind = "steal"
)

// CrossAction is published after the visitor has reserved its local resources.
type CrossAction struct {
	ReqID      uint64     `json:"req_id"`
	Kind       ActionKind `json:"kind"`
	VisitorUID uint64     `json:"visitor_uid"`
	OwnerUID   uint64     `json:"owner_uid"`
	PlotIndex  uint8      `json:"plot_index"`
}

// CrossResult is sent back to the visitor after the owner farm decides an action.
// Code is pkgerr.OK on success; any other code requires the visitor to roll back.
type CrossResult struct {
	ReqID      uint64      `json:"req_id"`
	VisitorUID uint64      `json:"visitor_uid"`
	OwnerUID   uint64      `json:"owner_uid"`
	Code       pkgerr.Code `json:"code"`
}

// PendingState describes the terminal or in-flight state of a visitor reservation.
type PendingState string

const (
	Reserved   PendingState = "reserved"
	Settled    PendingState = "settled"
	RolledBack PendingState = "rolled_back"
)
