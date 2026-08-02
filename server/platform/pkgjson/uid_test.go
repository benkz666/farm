package pkgjson

import (
	"encoding/json"
	"testing"
)

func TestUIDRoundTripAsString(t *testing.T) {
	const raw = uint64(1785142595526523238)
	b, err := json.Marshal(UID(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1785142595526523238"` {
		t.Fatalf("marshal = %s", b)
	}
	var got UID
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if uint64(got) != raw {
		t.Fatalf("got %d", got)
	}
}

func TestUIDUnmarshalNumber(t *testing.T) {
	var got UID
	if err := json.Unmarshal([]byte(`42`), &got); err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("got %d", got)
	}
}

func TestUint64AdjacentUnsafeIntegersMarshalAsDistinctStrings(t *testing.T) {
	t.Parallel()

	left, err := json.Marshal(Uint64(9_007_199_254_740_992))
	if err != nil {
		t.Fatalf("marshal left: %v", err)
	}
	right, err := json.Marshal(Uint64(9_007_199_254_740_993))
	if err != nil {
		t.Fatalf("marshal right: %v", err)
	}
	if string(left) != `"9007199254740992"` || string(right) != `"9007199254740993"` {
		t.Fatalf("left=%s right=%s", left, right)
	}
}

func TestInt64RoundTripAsString(t *testing.T) {
	t.Parallel()

	const raw = int64(9_007_199_254_740_993)
	encoded, err := json.Marshal(Int64(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `"9007199254740993"` {
		t.Fatalf("marshal = %s", encoded)
	}
	var got Int64
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if int64(got) != raw {
		t.Fatalf("got %d want %d", got, raw)
	}
}
