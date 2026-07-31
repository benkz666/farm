package cross

import (
	"encoding/json"
	"testing"
)

func TestVisitorRewardReqIDUsesDecimalString(t *testing.T) {
	t.Parallel()

	const reqID = uint64(18_446_744_073_709_551_615)
	encoded, err := json.Marshal(VisitorReward{ReqID: reqID, Amount: 1})
	if err != nil {
		t.Fatal(err)
	}
	const want = `"req_id":"18446744073709551615"`
	if !contains(encoded, []byte(want)) {
		t.Fatalf("json=%s, missing %s", encoded, want)
	}
	var decoded VisitorReward
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ReqID != reqID {
		t.Fatalf("req_id=%d want=%d", decoded.ReqID, reqID)
	}
}

func TestVisitorRewardCoinsUseDecimalStrings(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(VisitorReward{
		CoinGained:   9_007_199_254_740_993,
		Compensation: 9_007_199_254_740_992,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"coin_gained":"9007199254740993"`,
		`"compensation":"9007199254740992"`,
	} {
		if !contains(encoded, []byte(want)) {
			t.Fatalf("json=%s, missing %s", encoded, want)
		}
	}
}

func contains(haystack, needle []byte) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if string(haystack[index:index+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
