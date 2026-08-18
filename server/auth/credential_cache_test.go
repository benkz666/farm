package auth

import (
	"testing"
	"time"
)

func TestCredentialVerifyCacheRequiresExactCredentialAndHash(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newCredentialVerifyCache(time.Minute, 4, func() time.Time { return now })
	cache.remember("alice", "hash-a", "secret")

	if !cache.hit("alice", "hash-a", "secret") {
		t.Fatal("exact credential should hit")
	}
	if cache.hit("alice", "hash-a", "wrong") {
		t.Fatal("wrong password must not hit")
	}
	if cache.hit("alice", "hash-b", "secret") {
		t.Fatal("changed password hash must invalidate cached credential")
	}
	if cache.hit("bob", "hash-a", "secret") {
		t.Fatal("another account must not share cached credential")
	}
}

func TestCredentialVerifyCacheExpiresAndIsBoundedLRU(t *testing.T) {
	now := time.Unix(100, 0)
	cache := newCredentialVerifyCache(time.Minute, 2, func() time.Time { return now })
	cache.remember("a", "hash", "pw")
	cache.remember("b", "hash", "pw")
	if !cache.hit("a", "hash", "pw") {
		t.Fatal("a should hit before eviction")
	}
	cache.remember("c", "hash", "pw")
	if cache.hit("b", "hash", "pw") {
		t.Fatal("least recently used entry should be evicted")
	}
	if !cache.hit("a", "hash", "pw") || !cache.hit("c", "hash", "pw") {
		t.Fatal("recent entries should remain cached")
	}

	now = now.Add(time.Minute)
	if cache.hit("a", "hash", "pw") {
		t.Fatal("expired credential must not hit")
	}
}
