package store

import (
	"encoding/json"
	"testing"
)

func TestMailIDMarshalsAsDecimalString(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(Mail{
		ID:             9_007_199_254_740_993,
		Title:          "奖励",
		AttachmentCoin: 9_007_199_254_740_993,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"id":"9007199254740993"`,
		`"attachment_coin":"9007199254740993"`,
	} {
		if !containsBytes(encoded, []byte(want)) {
			t.Fatalf("json=%s, missing %s", encoded, want)
		}
	}
}

func TestTaskRewardsMarshalCoinAsDecimalString(t *testing.T) {
	t.Parallel()

	for _, value := range []any{
		Task{RewardCoin: 9_007_199_254_740_993},
		TaskReward{Coin: 9_007_199_254_740_993},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !containsBytes(encoded, []byte(`"coin":"9007199254740993"`)) &&
			!containsBytes(encoded, []byte(`"reward_coin":"9007199254740993"`)) {
			t.Fatalf("json=%s, expected decimal-string coin", encoded)
		}
	}
}

func containsBytes(haystack, needle []byte) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if string(haystack[index:index+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
