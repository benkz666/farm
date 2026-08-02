package farm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"farm/server/platform/pkgjson"
)

// 这些投影会直接进入浏览器协议。Go 内部继续使用 uint64 做状态机运算，
// 但 JSON 边界必须输出十进制字符串，避免被 JavaScript Number 合并相邻整数。

type farmSnapshotJSONAlias FarmSnapshotJSON

func (snapshot FarmSnapshotJSON) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		OwnerUID pkgjson.UID   `json:"owner_uid"`
		Coin     pkgjson.Int64 `json:"coin"`
		farmSnapshotJSONAlias
	}{
		OwnerUID:              pkgjson.UID(snapshot.OwnerUID),
		Coin:                  pkgjson.Int64(snapshot.Coin),
		farmSnapshotJSONAlias: farmSnapshotJSONAlias(snapshot),
	})
}

func (snapshot *FarmSnapshotJSON) UnmarshalJSON(data []byte) error {
	if snapshot == nil {
		return errors.New("farm: nil FarmSnapshotJSON")
	}
	wire := struct {
		OwnerUID pkgjson.UID   `json:"owner_uid"`
		Coin     pkgjson.Int64 `json:"coin"`
		*farmSnapshotJSONAlias
	}{
		farmSnapshotJSONAlias: (*farmSnapshotJSONAlias)(snapshot),
	}
	if err := decodeWireJSON(data, &wire); err != nil {
		return err
	}
	snapshot.OwnerUID = uint64(wire.OwnerUID)
	snapshot.Coin = int64(wire.Coin)
	return nil
}

type patchJSONAlias PatchJSON

func (patch PatchJSON) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		FarmSeq pkgjson.Uint64 `json:"farm_seq"`
		Coin    pkgjson.Int64  `json:"coin"`
		patchJSONAlias
	}{
		FarmSeq:        pkgjson.Uint64(patch.FarmSeq),
		Coin:           pkgjson.Int64(patch.Coin),
		patchJSONAlias: patchJSONAlias(patch),
	})
}

func (patch *PatchJSON) UnmarshalJSON(data []byte) error {
	if patch == nil {
		return errors.New("farm: nil PatchJSON")
	}
	wire := struct {
		FarmSeq pkgjson.Uint64 `json:"farm_seq"`
		Coin    pkgjson.Int64  `json:"coin"`
		*patchJSONAlias
	}{
		patchJSONAlias: (*patchJSONAlias)(patch),
	}
	if err := decodeWireJSON(data, &wire); err != nil {
		return err
	}
	patch.FarmSeq = uint64(wire.FarmSeq)
	patch.Coin = int64(wire.Coin)
	return nil
}

type playerDeltaJSONAlias PlayerDelta

func (delta PlayerDelta) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Coin pkgjson.Int64 `json:"coin"`
		playerDeltaJSONAlias
	}{
		Coin:                 pkgjson.Int64(delta.Coin),
		playerDeltaJSONAlias: playerDeltaJSONAlias(delta),
	})
}

func (delta *PlayerDelta) UnmarshalJSON(data []byte) error {
	if delta == nil {
		return errors.New("farm: nil PlayerDelta")
	}
	wire := struct {
		Coin pkgjson.Int64 `json:"coin"`
		*playerDeltaJSONAlias
	}{
		playerDeltaJSONAlias: (*playerDeltaJSONAlias)(delta),
	}
	if err := decodeWireJSON(data, &wire); err != nil {
		return err
	}
	delta.Coin = int64(wire.Coin)
	return nil
}

type codexRewardNoticeJSONAlias CodexRewardNotice

func (notice CodexRewardNotice) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		RewardCoin pkgjson.Int64 `json:"reward_coin"`
		codexRewardNoticeJSONAlias
	}{
		RewardCoin:                 pkgjson.Int64(notice.RewardCoin),
		codexRewardNoticeJSONAlias: codexRewardNoticeJSONAlias(notice),
	})
}

func (notice *CodexRewardNotice) UnmarshalJSON(data []byte) error {
	if notice == nil {
		return errors.New("farm: nil CodexRewardNotice")
	}
	wire := struct {
		RewardCoin pkgjson.Int64 `json:"reward_coin"`
		*codexRewardNoticeJSONAlias
	}{
		codexRewardNoticeJSONAlias: (*codexRewardNoticeJSONAlias)(notice),
	}
	if err := decodeWireJSON(data, &wire); err != nil {
		return err
	}
	notice.RewardCoin = int64(wire.RewardCoin)
	return nil
}

type farmDeltaAlias FarmDelta

func (delta FarmDelta) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		OwnerUID pkgjson.UID    `json:"owner_uid"`
		FarmSeq  pkgjson.Uint64 `json:"farm_seq"`
		ActorUID pkgjson.UID    `json:"actor_uid"`
		farmDeltaAlias
	}{
		OwnerUID:       pkgjson.UID(delta.OwnerUID),
		FarmSeq:        pkgjson.Uint64(delta.FarmSeq),
		ActorUID:       pkgjson.UID(delta.ActorUID),
		farmDeltaAlias: farmDeltaAlias(delta),
	})
}

func (delta *FarmDelta) UnmarshalJSON(data []byte) error {
	if delta == nil {
		return errors.New("farm: nil FarmDelta")
	}
	wire := struct {
		OwnerUID pkgjson.UID    `json:"owner_uid"`
		FarmSeq  pkgjson.Uint64 `json:"farm_seq"`
		ActorUID pkgjson.UID    `json:"actor_uid"`
		*farmDeltaAlias
	}{
		farmDeltaAlias: (*farmDeltaAlias)(delta),
	}
	if err := decodeWireJSON(data, &wire); err != nil {
		return err
	}
	delta.OwnerUID = uint64(wire.OwnerUID)
	delta.FarmSeq = uint64(wire.FarmSeq)
	delta.ActorUID = uint64(wire.ActorUID)
	return nil
}

func decodeWireJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("farm: trailing JSON value")
	}
	return nil
}
