package store

import "testing"

func TestParseFormatItemKeyRoundTrip(t *testing.T) {
	cases := []struct {
		kind   uint8
		itemID uint16
	}{
		{ItemKindSeed, 1},
		{ItemKindFertilizer, 2},
		{ItemKindDogFood, 1},
		{ItemKindFruit, 16},
	}
	for _, tc := range cases {
		key, err := FormatItemKey(tc.kind, tc.itemID)
		if err != nil {
			t.Fatalf("FormatItemKey(%d,%d): %v", tc.kind, tc.itemID, err)
		}
		gotKind, gotID, err := ParseItemKey(key)
		if err != nil {
			t.Fatalf("ParseItemKey(%q): %v", key, err)
		}
		if gotKind != tc.kind || gotID != tc.itemID {
			t.Fatalf("round-trip: got kind=%d id=%d, want kind=%d id=%d", gotKind, gotID, tc.kind, tc.itemID)
		}
	}
}

func TestParseItemKeyRejectsUnknown(t *testing.T) {
	if _, _, err := ParseItemKey("dogfood:2"); err == nil {
		t.Fatal("expected error for unsupported key")
	}
}
