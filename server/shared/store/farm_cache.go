package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/gameconfig"
	"farm/server/shared/outbox"

	"google.golang.org/protobuf/proto"
)

// farmCacheProtoMagic versions the Redis value independently from the MySQL
// schema and from the public client protocol. Values without this header are
// treated as cache misses and rebuilt from MySQL.
var farmCacheProtoMagic = []byte{'F', 'M', 'P', 'B', 1}

func encodeFarmCache(agg *farm.Aggregate) ([]byte, error) {
	if agg == nil || agg.UID == 0 {
		return nil, errors.New("store: invalid farm cache aggregate")
	}
	mutation, err := outbox.NewFarmWriteMutation(
		agg,
		outbox.PersistPlan{Mode: outbox.PersistFull},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("store: build farm cache protobuf: %w", err)
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(mutation)
	if err != nil {
		return nil, fmt.Errorf("store: marshal farm cache protobuf: %w", err)
	}
	payload := make([]byte, len(farmCacheProtoMagic)+len(body))
	copy(payload, farmCacheProtoMagic)
	copy(payload[len(farmCacheProtoMagic):], body)
	return payload, nil
}

func decodeFarmCache(payload []byte) (*farm.Aggregate, error) {
	if !bytes.HasPrefix(payload, farmCacheProtoMagic) {
		return nil, errors.New("store: unsupported farm cache encoding")
	}

	var mutation farmv1.FarmWriteMutation
	if err := proto.Unmarshal(payload[len(farmCacheProtoMagic):], &mutation); err != nil {
		return nil, fmt.Errorf("store: decode farm cache protobuf: %w", err)
	}
	return aggregateFromFarmCacheMutation(&mutation)
}

func aggregateFromFarmCacheMutation(mutation *farmv1.FarmWriteMutation) (*farm.Aggregate, error) {
	if mutation == nil || mutation.Uid == 0 ||
		mutation.PlayerMask&outbox.PlayerAll != outbox.PlayerAll ||
		!mutation.ReplaceItems || !mutation.ReplaceCodex {
		return nil, errors.New("store: incomplete farm cache protobuf")
	}
	if mutation.Level > math.MaxUint16 || mutation.UnlockedPlots > math.MaxUint8 {
		return nil, errors.New("store: farm cache player field overflow")
	}
	agg := &farm.Aggregate{
		UID:           mutation.Uid,
		Nickname:      mutation.Nickname,
		Level:         uint16(mutation.Level),
		Exp:           mutation.Exp,
		Coin:          mutation.Coin,
		UnlockedPlots: uint8(mutation.UnlockedPlots),
		FarmSeq:       mutation.FarmSeq,
		Items:         make(map[farm.ItemKey]uint32, len(mutation.Items)),
		CodexHarvests: make(map[uint16]uint32, len(mutation.Codex)),
	}
	if err := decodeFarmCacheJSONFields(agg, mutation); err != nil {
		return nil, err
	}

	if len(mutation.Plots) != gameconfig.MaxPlots {
		return nil, fmt.Errorf("store: farm cache plot count %d want %d", len(mutation.Plots), gameconfig.MaxPlots)
	}
	seenPlots := [gameconfig.MaxPlots]bool{}
	for _, encoded := range mutation.Plots {
		if encoded == nil || encoded.Index >= gameconfig.MaxPlots || seenPlots[encoded.Index] {
			return nil, errors.New("store: invalid farm cache plot")
		}
		if encoded.State > math.MaxUint8 || encoded.SeasonIndex > math.MaxUint8 ||
			encoded.SeasonTotal > math.MaxUint8 || encoded.StageCount > math.MaxUint8 ||
			encoded.FertMask > math.MaxUint8 || encoded.WeedNextWin > math.MaxUint8 ||
			encoded.PestNextWin > math.MaxUint8 || encoded.CropId > math.MaxUint16 ||
			encoded.FinalYield > math.MaxUint16 || encoded.StolenCount > math.MaxUint16 {
			return nil, errors.New("store: farm cache plot field overflow")
		}
		seenPlots[encoded.Index] = true
		agg.Plots[encoded.Index] = decodeMutationPlot(encoded)
	}
	for _, encoded := range mutation.Items {
		if encoded == nil || encoded.Key == "" {
			return nil, errors.New("store: invalid farm cache item")
		}
		key := farm.ItemKey(encoded.Key)
		if _, duplicate := agg.Items[key]; duplicate {
			return nil, errors.New("store: duplicate farm cache item")
		}
		if encoded.Count != 0 {
			agg.Items[key] = encoded.Count
		}
	}
	for _, encoded := range mutation.Codex {
		if encoded == nil || encoded.CropId == 0 || encoded.CropId > math.MaxUint16 {
			return nil, errors.New("store: invalid farm cache codex row")
		}
		cropID := uint16(encoded.CropId)
		if _, duplicate := agg.CodexHarvests[cropID]; duplicate {
			return nil, errors.New("store: duplicate farm cache codex row")
		}
		if encoded.HarvestCount != 0 {
			agg.CodexHarvests[cropID] = encoded.HarvestCount
		}
	}
	return agg, nil
}

func decodeFarmCacheJSONFields(agg *farm.Aggregate, mutation *farmv1.FarmWriteMutation) error {
	if len(mutation.DailyJson) > 0 {
		if err := json.Unmarshal(mutation.DailyJson, &agg.Daily); err != nil {
			return fmt.Errorf("store: decode farm cache daily state: %w", err)
		}
	}
	if len(mutation.PetJson) > 0 {
		if err := json.Unmarshal(mutation.PetJson, &agg.Pet); err != nil {
			return fmt.Errorf("store: decode farm cache pet state: %w", err)
		}
	}
	if len(mutation.CrossPendingJson) > 0 {
		if err := json.Unmarshal(mutation.CrossPendingJson, &agg.CrossPending); err != nil {
			return fmt.Errorf("store: decode farm cache cross pending: %w", err)
		}
	}
	if len(mutation.CrossReceiptJson) > 0 {
		if err := json.Unmarshal(mutation.CrossReceiptJson, &agg.CrossReceipts); err != nil {
			return fmt.Errorf("store: decode farm cache cross receipts: %w", err)
		}
	}
	return nil
}
