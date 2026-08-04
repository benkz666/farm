package farm

import (
	"encoding/json"
	"testing"
)

func TestClientWireUint64FieldsAreDecimalStrings(t *testing.T) {
	t.Parallel()

	const left = uint64(9_007_199_254_740_992)
	const right = uint64(9_007_199_254_740_993)
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "snapshot uid",
			value: FarmSnapshotJSON{OwnerUID: right},
			want:  `{"owner_uid":"9007199254740993"`,
		},
		{
			name:  "patch sequence",
			value: PatchJSON{FarmSeq: right},
			want:  `"farm_seq":"9007199254740993"`,
		},
		{
			name:  "snapshot coin",
			value: FarmSnapshotJSON{Coin: 9_007_199_254_740_993},
			want:  `"coin":"9007199254740993"`,
		},
		{
			name:  "patch coin",
			value: PatchJSON{Coin: 9_007_199_254_740_993},
			want:  `"coin":"9007199254740993"`,
		},
		{
			name:  "player delta coin",
			value: PlayerDelta{Coin: 9_007_199_254_740_993},
			want:  `"coin":"9007199254740993"`,
		},
		{
			name:  "codex reward coin",
			value: CodexRewardNotice{RewardCoin: 9_007_199_254_740_993},
			want:  `"reward_coin":"9007199254740993"`,
		},
		{
			name: "delta identities and sequence",
			value: FarmDelta{
				OwnerUID: left,
				FarmSeq:  right,
				ActorUID: right,
			},
			want: `"owner_uid":"9007199254740992","farm_seq":"9007199254740993","actor_uid":"9007199254740993"`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if !bytesContains(encoded, []byte(tc.want)) {
				t.Fatalf("json=%s, missing %s", encoded, tc.want)
			}
		})
	}
}

func TestFarmDeltaWireRoundTripPreservesAdjacentUnsafeIntegers(t *testing.T) {
	t.Parallel()

	want := FarmDelta{
		OwnerUID: 9_007_199_254_740_992,
		FarmSeq:  9_007_199_254_740_993,
		ActorUID: 18_446_744_073_709_551_615,
		Action:   212,
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got FarmDelta
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.OwnerUID != want.OwnerUID ||
		got.FarmSeq != want.FarmSeq ||
		got.ActorUID != want.ActorUID ||
		got.Action != want.Action {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

func bytesContains(haystack, needle []byte) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if string(haystack[index:index+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
