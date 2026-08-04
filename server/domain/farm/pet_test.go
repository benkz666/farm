package farm

import (
	"encoding/json"
	"testing"

	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
)

func TestPetEmptyBowlCannotInterceptAndFedDogUsesConfiguredRate(t *testing.T) {
	agg := NewAggregate(1, "owner")
	agg.Coin = 3_000

	if result := agg.Buy(BuyReq{ItemID: DogMuttShopItemID, Quantity: 1}); result.Err != errcode.OK {
		t.Fatalf("Buy dog Err = %d, want OK", result.Err)
	}
	if result := agg.PetActivate(DogMutt, 1_000); result.Err != errcode.OK {
		t.Fatalf("PetActivate Err = %d, want OK", result.Err)
	}
	if agg.Pet.ShouldIntercept(1_000, 0) {
		t.Fatal("empty bowl intercepted a steal")
	}

	agg.Items[DogFoodItem()] = 4
	if result := agg.PetFeed(PetFeedReq{Grams: 4}, 1_000); result.Err != errcode.OK {
		t.Fatalf("PetFeed Err = %d, want OK", result.Err)
	}
	if !agg.Pet.ShouldIntercept(1_000, 0) {
		t.Fatal("fed mutt did not intercept a guaranteed roll")
	}
	if agg.Pet.ShouldIntercept(1_000+MuttMsPerGram*4, 0) {
		t.Fatal("dog intercepted after its bowl became empty")
	}
}

func TestAllDogBreedsRespectUnlockPriceAndIndependentOwnership(t *testing.T) {
	agg := NewAggregate(1, "owner")
	agg.Coin = 20_000
	agg.Level = 9
	beforeCoin, beforeSeq := agg.Coin, agg.FarmSeq

	if result := agg.Buy(BuyReq{ItemID: DogShepherdShopItemID, Quantity: 1}); result.Err != errcode.DogLocked {
		t.Fatalf("locked shepherd buy Err = %d, want %d", result.Err, errcode.DogLocked)
	}
	if agg.Coin != beforeCoin || agg.FarmSeq != beforeSeq || agg.Pet.HasDog(DogShepherd) {
		t.Fatal("locked shepherd purchase mutated aggregate")
	}

	agg.Level = 10
	if result := agg.Buy(BuyReq{ItemID: DogShepherdShopItemID, Quantity: 1}); result.Err != errcode.OK {
		t.Fatalf("shepherd buy Err = %d, want OK", result.Err)
	}
	if !agg.Pet.HasDog(DogShepherd) || agg.Coin != beforeCoin-4_500 {
		t.Fatalf("shepherd purchase state = %#v, coin=%d", agg.Pet, agg.Coin)
	}

	agg.Level = 20
	if result := agg.Buy(BuyReq{ItemID: DogMastiffShopItemID, Quantity: 1}); result.Err != errcode.OK {
		t.Fatalf("mastiff buy Err = %d, want OK", result.Err)
	}
	if !agg.Pet.HasDog(DogMastiff) || agg.Coin != beforeCoin-4_500-8_000 {
		t.Fatalf("mastiff purchase state = %#v, coin=%d", agg.Pet, agg.Coin)
	}
	if result := agg.Buy(BuyReq{ItemID: DogMastiffShopItemID, Quantity: 1}); result.Err != errcode.DogAlreadyOwned {
		t.Fatalf("duplicate mastiff buy Err = %d, want %d", result.Err, errcode.DogAlreadyOwned)
	}
}

func TestSwitchingBreedPreservesFoodAndAppliesNewConsumptionRate(t *testing.T) {
	agg := NewAggregate(1, "owner")
	agg.Pet.Owned = 0b111
	agg.Items[DogFoodItem()] = 10

	if result := agg.PetActivate(DogMutt, 1_000); result.Err != errcode.OK {
		t.Fatalf("activate mutt Err = %d", result.Err)
	}
	if result := agg.PetFeed(PetFeedReq{Grams: 10}, 1_000); result.Err != errcode.OK {
		t.Fatalf("feed mutt Err = %d", result.Err)
	}
	if agg.Pet.BowlEmptyAt != 16_000 || agg.Pet.MsPerGram != MuttMsPerGram {
		t.Fatalf("mutt bowl = %#v", agg.Pet)
	}

	if result := agg.PetActivate(DogShepherd, 4_000); result.Err != errcode.OK {
		t.Fatalf("activate shepherd Err = %d", result.Err)
	}
	if got := agg.Pet.remainingGrams(4_000); got != 8 {
		t.Fatalf("shepherd remaining grams = %d, want 8", got)
	}
	if agg.Pet.BowlEmptyAt != 13_600 || agg.Pet.MsPerGram != ShepherdMsPerGram {
		t.Fatalf("shepherd bowl = %#v", agg.Pet)
	}

	if result := agg.PetActivate(DogMastiff, 5_200); result.Err != errcode.OK {
		t.Fatalf("activate mastiff Err = %d", result.Err)
	}
	if got := agg.Pet.remainingGrams(5_200); got != 7 {
		t.Fatalf("mastiff remaining grams = %d, want 7", got)
	}
	if agg.Pet.BowlEmptyAt != 5_200+7*MastiffMsPerGram || agg.Pet.MsPerGram != MastiffMsPerGram {
		t.Fatalf("mastiff bowl = %#v", agg.Pet)
	}
}

func TestPetFeedUsesAuthoritativeTimeProfile(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Pet.Owned = 1 << uint(DogMutt-1)
	agg.Pet.ActiveDog = DogMutt
	agg.Items[DogFoodItem()] = 4

	result := agg.PetFeedWithProfile(PetFeedReq{Grams: 4}, 1_000, gameconfig.TimeProfileAuthentic)
	if result.Err != errcode.OK {
		t.Fatalf("PetFeedWithProfile Err = %d, want OK", result.Err)
	}
	wantRate := gameconfig.HourMs(gameconfig.TimeProfileAuthentic) / 4
	if agg.Pet.MsPerGram != wantRate || agg.Pet.BowlEmptyAt != 1_000+4*wantRate {
		t.Fatalf("pet timing = rate:%d empty:%d, want rate:%d empty:%d", agg.Pet.MsPerGram, agg.Pet.BowlEmptyAt, wantRate, 1_000+4*wantRate)
	}
}

func TestDogBreedsGrowAndInterceptIndependently(t *testing.T) {
	pet := PetState{Owned: 0b111, ActiveDog: DogShepherd}
	for range 20 {
		pet.recordIntercept()
	}
	status := pet.Status(0)
	if status.DogLevel != 1 || status.Intercepts != 20 || status.InterceptionPct != 36 {
		t.Fatalf("active shepherd status = %#v", status)
	}

	pet.ActiveDog = DogMastiff
	status = pet.Status(0)
	if status.DogLevel != 0 || status.Intercepts != 0 || status.InterceptionPct != 45 {
		t.Fatalf("inactive-growth mastiff status = %#v", status)
	}
	if len(status.Dogs) != 3 || status.Dogs[1].DogType != DogShepherd ||
		status.Dogs[1].Level != 1 || status.Dogs[1].Intercepts != 20 {
		t.Fatalf("all dog states = %#v", status.Dogs)
	}
}

func TestLegacySingleDogPetJSONRemainsCompatible(t *testing.T) {
	var pet PetState
	if err := json.Unmarshal([]byte(`{
		"active_dog": 1,
		"owned": 1,
		"dog_level": [2],
		"intercepts": [40],
		"bowl_empty_at": 5000,
		"ms_per_gram": 1500
	}`), &pet); err != nil {
		t.Fatalf("decode legacy pet: %v", err)
	}
	if pet.DogLevel != [3]uint8{2, 0, 0} || pet.Intercepts != [3]uint16{40, 0, 0} {
		t.Fatalf("legacy pet arrays = levels %#v intercepts %#v", pet.DogLevel, pet.Intercepts)
	}
	if !pet.HasDog(DogMutt) || pet.HasDog(DogShepherd) {
		t.Fatalf("legacy ownership = %08b", pet.Owned)
	}
}

func TestStealCompensationFreezeTransfersOnlyOnInterception(t *testing.T) {
	visitor := NewAggregate(2, "visitor")
	owner := NewAggregate(1, "owner")
	visitor.Coin = 170

	if got := visitor.FreezeStealCompensation(170); got != errcode.OK {
		t.Fatalf("FreezeStealCompensation = %d, want OK", got)
	}
	owner.ReceiveStealCompensation(170)
	if visitor.Coin != 0 || owner.Coin != gameconfig.InitialCoin+170 {
		t.Fatalf("after interception visitor=%d owner=%d", visitor.Coin, owner.Coin)
	}

	visitor.Coin = 169
	if got := visitor.FreezeStealCompensation(170); got != errcode.StealNoAfford {
		t.Fatalf("FreezeStealCompensation insufficient = %d, want %d", got, errcode.StealNoAfford)
	}
	if visitor.Coin != 169 {
		t.Fatalf("freeze mutated insufficient visitor coin = %d", visitor.Coin)
	}
}
