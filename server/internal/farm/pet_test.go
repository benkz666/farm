package farm

import (
	"testing"

	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

func TestPetEmptyBowlCannotInterceptAndFedDogUsesConfiguredRate(t *testing.T) {
	agg := NewAggregate(1, "owner")
	agg.Coin = 3_000

	if result := agg.Buy(BuyReq{ItemID: DogMuttShopItemID, Quantity: 1}); result.Err != pkgerr.OK {
		t.Fatalf("Buy dog Err = %d, want OK", result.Err)
	}
	if result := agg.PetActivate(DogMutt); result.Err != pkgerr.OK {
		t.Fatalf("PetActivate Err = %d, want OK", result.Err)
	}
	if agg.Pet.ShouldIntercept(1_000, 0) {
		t.Fatal("empty bowl intercepted a steal")
	}

	agg.Items[DogFoodItem()] = 4
	if result := agg.PetFeed(PetFeedReq{Grams: 4}, 1_000); result.Err != pkgerr.OK {
		t.Fatalf("PetFeed Err = %d, want OK", result.Err)
	}
	if !agg.Pet.ShouldIntercept(1_000, 0) {
		t.Fatal("fed mutt did not intercept a guaranteed roll")
	}
	if agg.Pet.ShouldIntercept(1_000+MuttMsPerGram*4, 0) {
		t.Fatal("dog intercepted after its bowl became empty")
	}
}

func TestStealCompensationFreezeTransfersOnlyOnInterception(t *testing.T) {
	visitor := NewAggregate(2, "visitor")
	owner := NewAggregate(1, "owner")
	visitor.Coin = 170

	if got := visitor.FreezeStealCompensation(170); got != pkgerr.OK {
		t.Fatalf("FreezeStealCompensation = %d, want OK", got)
	}
	owner.ReceiveStealCompensation(170)
	if visitor.Coin != 0 || owner.Coin != gameconf.InitialCoin+170 {
		t.Fatalf("after interception visitor=%d owner=%d", visitor.Coin, owner.Coin)
	}

	visitor.Coin = 169
	if got := visitor.FreezeStealCompensation(170); got != pkgerr.StealNoAfford {
		t.Fatalf("FreezeStealCompensation insufficient = %d, want %d", got, pkgerr.StealNoAfford)
	}
	if visitor.Coin != 169 {
		t.Fatalf("freeze mutated insufficient visitor coin = %d", visitor.Coin)
	}
}
