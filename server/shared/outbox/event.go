package outbox

import (
	"fmt"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"

	"google.golang.org/protobuf/proto"
)

// Kind identifies durable outbox event types.
type Kind string

const (
	KindCrossResult Kind = "cross_result"
)

// Event is a deterministic, protobuf-backed outbox entry.
type Event struct {
	EventID     string
	ProducerUID uint64
	TargetUID   uint64
	Kind        Kind
	Payload     []byte
}

// FarmCommit bundles one immutable snapshot with durable outbox events that must
// land in the same MySQL transaction.
type FarmCommit struct {
	Snapshot      *farm.Aggregate
	Mutation      *farmv1.FarmWriteMutation
	Outbox        []Event
	TaskAdvances  []TaskAdvance
	CodexRewards  []CodexReward
	TaskClaims    []TaskClaim
	MailMutations []MailMutation
	Plan          PersistPlan
}

// TaskAdvance and CodexReward ride in the same durable Redis record as the
// authoritative Farm mutation. This keeps one gameplay command at one XADD
// barrier while the MySQL projector still applies each side effect idempotently.
type TaskAdvance struct {
	DayKey int64  `json:"day_key"`
	TaskID uint32 `json:"task_id"`
	Amount uint32 `json:"amount"`
}

type CodexReward struct {
	Progress farm.CodexProgress `json:"progress"`
}

// TaskClaim and MailMutation are Actor-authoritative state transitions. They
// share the Farm commit's Redis durability boundary; MySQL is only an eventual,
// idempotent projection of these facts.
type TaskClaim struct {
	DayKey    int64
	TaskID    uint32
	ClaimedAt int64
}

type MailMutationKind uint8

const (
	MailRead MailMutationKind = iota + 1
	MailClaim
	MailDelete
)

type MailMutation struct {
	MailID     uint64
	Kind       MailMutationKind
	OccurredAt int64
}

// PersistMode identifies the smallest safe row set for one aggregate commit.
// The zero value is deliberately the conservative full-snapshot mode.
type PersistMode uint8

const (
	PersistFull PersistMode = iota
	PersistEconomy
	PersistPlot
	PersistCrossVisitor
	PersistCrossOwner
	PersistSideEffects
)

// PersistPlan travels through the Actor committer so reduced writes preserve
// the same per-UID ordering and batching guarantees as full snapshots.
type PersistPlan struct {
	Mode PersistMode
	// Modes retains the exact union when legacy plan.Mode has to fall back to
	// PersistFull. Incremental Protobuf projection can still update only the
	// combined row set.
	Modes        uint32
	PlotIndex    uint8
	IncludeItems bool
	IncludeCodex bool
}

func PersistModeMask(mode PersistMode) uint32 { return 1 << uint32(mode) }

func CrossResultEventID(ownerUID, visitorUID, reqID uint64) string {
	return fmt.Sprintf("cross_result:%d:%d:%d", ownerUID, visitorUID, reqID)
}

// NewCrossResultEvent marshals a typed CrossResult for durable fan-out.
func NewCrossResultEvent(ownerUID uint64, result *farmv1.CrossResult) (Event, error) {
	if result == nil || result.ReqId == 0 || result.VisitorUid == 0 || result.OwnerUid == 0 {
		return Event{}, fmt.Errorf("outbox: invalid cross result")
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(result)
	if err != nil {
		return Event{}, fmt.Errorf("outbox: marshal cross result: %w", err)
	}
	return Event{
		EventID:     CrossResultEventID(ownerUID, result.VisitorUid, result.ReqId),
		ProducerUID: ownerUID,
		TargetUID:   result.VisitorUid,
		Kind:        KindCrossResult,
		Payload:     payload,
	}, nil
}

// DecodeCrossResult unmarshals a cross-result payload.
func DecodeCrossResult(payload []byte) (*farmv1.CrossResult, error) {
	result := &farmv1.CrossResult{}
	if err := proto.Unmarshal(payload, result); err != nil {
		return nil, fmt.Errorf("outbox: unmarshal cross result: %w", err)
	}
	return result, nil
}
