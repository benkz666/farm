package cross

import (
	"errors"
	"sync"
	"time"

	"farm/server/internal/pkgerr"
)

// PendingTimeout is the maximum time a visitor-side reservation can wait for a
// CrossResult before its local reservation must be rolled back.
const PendingTimeout = 5 * time.Second

var (
	ErrInvalidAction  = errors.New("cross: invalid action")
	ErrAlreadyPending = errors.New("cross: request already pending")
	ErrResultMismatch = errors.New("cross: result does not match reservation")
)

// Pending is a visitor-side reservation. Terminal states are returned by Settle
// and Expire so the owning Actor can apply a reward or undo its local reserve.
type Pending struct {
	Action     CrossAction
	State      PendingState
	ReservedAt time.Time
	FinishedAt time.Time
	Result     CrossResult
}

// Visitor manages the reserved → settled|rolled-back state machine for one
// visitor farm. Its methods are safe for concurrent EventBus deliveries, though
// callers still apply resulting farm mutations on their visitor Actor.
type Visitor struct {
	mu      sync.Mutex
	pending map[uint64]Pending
	now     func() time.Time
}

// NewVisitor constructs a visitor-side state machine using the system clock.
func NewVisitor() *Visitor {
	return &Visitor{
		pending: make(map[uint64]Pending),
		now:     time.Now,
	}
}

// Reserve records a local pre-reservation before CrossAction is published.
func (v *Visitor) Reserve(action CrossAction) (Pending, error) {
	if v == nil {
		return Pending{}, errors.New("cross: visitor is nil")
	}
	if action.ReqID == 0 || action.Kind == "" || action.VisitorUID == 0 || action.OwnerUID == 0 {
		return Pending{}, ErrInvalidAction
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.pending[action.ReqID]; exists {
		return Pending{}, ErrAlreadyPending
	}

	pending := Pending{
		Action:     action,
		State:      Reserved,
		ReservedAt: v.now(),
	}
	v.pending[action.ReqID] = pending
	return pending, nil
}

// Pending returns the in-flight reservation for reqID, if any.
func (v *Visitor) Pending(reqID uint64) (Pending, bool) {
	if v == nil {
		return Pending{}, false
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	pending, ok := v.pending[reqID]
	return pending, ok
}

// Settle applies one CrossResult. Repeated and unknown results are no-ops so
// at-least-once EventBus delivery cannot grant a visitor reward twice.
func (v *Visitor) Settle(result CrossResult) (Pending, bool, error) {
	if v == nil {
		return Pending{}, false, errors.New("cross: visitor is nil")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	pending, ok := v.pending[result.ReqID]
	if !ok {
		return Pending{}, false, nil
	}
	if result.VisitorUID != pending.Action.VisitorUID || result.OwnerUID != pending.Action.OwnerUID {
		return Pending{}, false, ErrResultMismatch
	}

	delete(v.pending, result.ReqID)
	pending.Result = result
	pending.FinishedAt = v.now()
	if result.Code == pkgerr.OK {
		pending.State = Settled
	} else {
		pending.State = RolledBack
	}
	return pending, true, nil
}

// Expire rolls back every reservation that waited at least PendingTimeout for a
// CrossResult. The returned records carry pkgerr.Timeout for client responses.
func (v *Visitor) Expire(now time.Time) []Pending {
	if v == nil {
		return nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	var expired []Pending
	for reqID, pending := range v.pending {
		if now.Before(pending.ReservedAt.Add(PendingTimeout)) {
			continue
		}
		delete(v.pending, reqID)
		pending.State = RolledBack
		pending.FinishedAt = now
		pending.Result = CrossResult{
			ReqID:      pending.Action.ReqID,
			VisitorUID: pending.Action.VisitorUID,
			OwnerUID:   pending.Action.OwnerUID,
			Code:       pkgerr.Timeout,
		}
		expired = append(expired, pending)
	}
	return expired
}
