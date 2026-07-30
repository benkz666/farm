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
