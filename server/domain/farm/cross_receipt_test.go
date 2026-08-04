package farm

import "testing"

func TestCrossReceiptSurvivesActorReplacementAndExpires(t *testing.T) {
	agg := NewAggregate(9, "owner")
	receipt := CrossReceipt{
		ReqID:      88,
		VisitorUID: 7,
		OwnerUID:   9,
		Code:       0,
		CropID:     1,
		Amount:     3,
		CreatedAt:  1,
	}
	agg.RecordCrossReceipt(receipt, 1_000)

	got, ok := agg.FindCrossReceipt(88, 7, 9, 1_001)
	if !ok || got.Amount != 3 || got.CropID != 1 {
		t.Fatalf("receipt = %#v, ok=%t", got, ok)
	}
	if _, ok := agg.FindCrossReceipt(88, 8, 9, 1_001); ok {
		t.Fatal("receipt must not be returned for a different visitor")
	}
	if _, ok := agg.FindCrossReceipt(88, 7, 9, 1_000+CrossReceiptTTL); ok {
		t.Fatal("expired receipt must not be replayed")
	}
}

func TestCrossReceiptCapacityCoversSupportedFriendBurst(t *testing.T) {
	const supportedFriendLimit = 200
	if maxCrossReceipts < supportedFriendLimit {
		t.Fatalf("maxCrossReceipts = %d, must cover %d friends", maxCrossReceipts, supportedFriendLimit)
	}

	agg := NewAggregate(9, "owner")
	for i := 0; i <= maxCrossReceipts; i++ {
		agg.RecordCrossReceipt(CrossReceipt{
			ReqID:      uint64(i + 1),
			VisitorUID: uint64(i + 100),
			OwnerUID:   9,
		}, int64(i+1))
	}
	if got := len(agg.CrossReceipts); got != maxCrossReceipts {
		t.Fatalf("receipt count = %d, want %d", got, maxCrossReceipts)
	}
	if _, ok := agg.CrossReceipts[1]; ok {
		t.Fatal("oldest receipt should be evicted after capacity is exceeded")
	}
}
