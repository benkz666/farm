package farm

import (
	"testing"

	"farm/server/internal/pkgerr"
)

func TestBuyWhiteRadishSeed(t *testing.T) {
	agg := NewAggregate(1, "alice")

	result := agg.Buy(BuyReq{ItemID: 1, Quantity: 1})
	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	if agg.Coin != 875 {
		t.Fatalf("Coin = %d, want 875", agg.Coin)
	}
	if got := agg.Items[SeedItem(1)]; got != 1 {
		t.Fatalf("seed count = %d, want 1", got)
	}
}

func TestBuyFertilizerAddsFertilizerItem(t *testing.T) {
	agg := NewAggregate(1, "alice")

	result := agg.Buy(BuyReq{ItemID: 1_001, Quantity: 1})

	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	if agg.Coin != 950 {
		t.Fatalf("Coin = %d, want 950", agg.Coin)
	}
	if got := agg.Items[FertilizerItem(1)]; got != 1 {
		t.Fatalf("fertilizer count = %d, want 1", got)
	}
}

func TestBuyNotEnoughCoin(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Coin = 100

	result := agg.Buy(BuyReq{ItemID: 1, Quantity: 1})
	if result.Err != pkgerr.NotEnoughCoin {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.NotEnoughCoin)
	}
	if agg.Coin != 100 {
		t.Fatalf("Coin mutated on failure: %d", agg.Coin)
	}
	if agg.Items[SeedItem(1)] != 0 {
		t.Fatal("seed granted on failure")
	}
}

func TestBuyCropLocked(t *testing.T) {
	agg := NewAggregate(1, "alice")
	// 大白菜 UnlockLevel=1
	result := agg.Buy(BuyReq{ItemID: 3, Quantity: 1})
	if result.Err != pkgerr.CropLocked {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.CropLocked)
	}
}

func TestBuyHiddenCropRejected(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Level = 20
	agg.Coin = 100_000
	// 人参 ID=27：隐藏作物不可商店购买
	result := agg.Buy(BuyReq{ItemID: 27, Quantity: 1})
	if result.Err != pkgerr.ItemNotFound {
		t.Fatalf("Buy hidden Err=%d, want ItemNotFound", result.Err)
	}
	if _, ok := agg.Items[SeedItem(27)]; ok {
		t.Fatalf("hidden seed should not be granted")
	}
}

func TestSellFruitAddsCoin(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Items[FruitItem(1)] = 2

	result := agg.Sell(SellReq{ItemID: 1, Quantity: 2})
	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	// 1000 + 2*17
	if agg.Coin != 1034 {
		t.Fatalf("Coin = %d, want 1034", agg.Coin)
	}
	if _, ok := agg.Items[FruitItem(1)]; ok {
		t.Fatalf("fruit still present: %d", agg.Items[FruitItem(1)])
	}
}

func TestSellNotEnoughItem(t *testing.T) {
	agg := NewAggregate(1, "alice")
	before := agg.Coin

	result := agg.Sell(SellReq{ItemID: 1, Quantity: 1})
	if result.Err != pkgerr.NotEnoughItem {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.NotEnoughItem)
	}
	if agg.Coin != before {
		t.Fatalf("Coin mutated on failure: %d", agg.Coin)
	}
}
