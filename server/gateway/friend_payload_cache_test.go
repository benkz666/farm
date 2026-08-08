package gateway

import (
	"bytes"
	"testing"

	"farm/server/shared/clientjson"
)

func TestFriendPayloadCacheReencodesChangedHint(t *testing.T) {
	cache := &friendPayloadCache{}
	items := []friendJSON{{UID: clientjson.UID(2), Nickname: "peer"}}
	before := cache.friendList(1, items)
	items[0].HasStealable = true
	after := cache.friendList(1, items)
	if bytes.Equal(before, after) {
		t.Fatalf("changed hint reused stale payload: %s", after)
	}
}

func TestFriendPayloadCacheReencodesChangedSearchResult(t *testing.T) {
	cache := &friendPayloadCache{}
	before := cache.search("alice", searchUserResponseItem{UID: clientjson.UID(1), Nickname: "old"})
	after := cache.search("alice", searchUserResponseItem{UID: clientjson.UID(1), Nickname: "new"})
	if bytes.Equal(before, after) {
		t.Fatalf("changed search result reused stale payload: %s", after)
	}
}
