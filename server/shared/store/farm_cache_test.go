package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"farm/server/domain/farm"
)

func TestFarmCacheProtobufRoundTrip(t *testing.T) {
	agg := farm.NewAggregate(9_007_199_254_740_993, "cache-user")
	agg.Level = 12
	agg.Exp = 3456
	agg.Coin = 9_007_199_254_740_991
	agg.UnlockedPlots = 18
	agg.FarmSeq = 9_007_199_254_740_995
	agg.Items[farm.SeedItem(2)] = 17
	agg.Items[farm.FruitItem(3)] = 8
	agg.CodexHarvests[3] = 21
	agg.Daily = farm.DailyState{DayID: 20260812, MaintainCnt: 7}
	agg.Pet = farm.PetState{ActiveDog: farm.DogMutt, Owned: 1, BowlEmptyAt: 1234567, MsPerGram: 1500}
	agg.CrossPending = map[uint64]farm.CrossReservation{
		9_007_199_254_740_997: {ReqID: 9_007_199_254_740_997, OwnerUID: 88, ReservedAt: 123},
	}
	agg.CrossReceipts = map[uint64]farm.CrossReceipt{
		9_007_199_254_740_999: {ReqID: 9_007_199_254_740_999, VisitorUID: 77, OwnerUID: agg.UID, CreatedAt: 456},
	}
	agg.HazardSalt = 12345

	payload, err := encodeFarmCache(agg)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= len(farmCacheProtoMagic) || string(payload[:len(farmCacheProtoMagic)]) != string(farmCacheProtoMagic) {
		t.Fatalf("farm cache payload does not have protobuf magic: %x", payload)
	}
	decoded, err := decodeFarmCache(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := agg.Clone()
	want.HazardSalt = 0
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded aggregate mismatch\n got: %#v\nwant: %#v", decoded, want)
	}
}

func TestFarmCacheDecoderRejectsLegacyJSON(t *testing.T) {
	agg := farm.NewAggregate(42, "legacy")
	agg.Items[farm.FruitItem(1)] = 3
	payload, err := json.Marshal(agg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeFarmCache(payload); err == nil {
		t.Fatal("legacy JSON farm cache was accepted")
	}
}

func TestFarmCacheDecoderRejectsIncompleteProtobuf(t *testing.T) {
	payload := append(append([]byte(nil), farmCacheProtoMagic...), 0x08, 0x2a)
	if _, err := decodeFarmCache(payload); err == nil {
		t.Fatal("incomplete protobuf farm cache was accepted")
	}
}
