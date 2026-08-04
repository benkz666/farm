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
	Snapshot *farm.Aggregate
	Outbox   []Event
}
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
