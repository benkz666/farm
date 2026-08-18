package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"
)

const (
	defaultCredentialCacheTTL      = 5 * time.Minute
	defaultCredentialCacheCapacity = 4096
)

type credentialCacheEntry struct {
	expiresAt time.Time
	lastUsed  uint64
}

// credentialVerifyCache 只缓存已经通过 bcrypt 的完整凭据指纹。指纹由进程级
// 随机 HMAC key 生成，不保留明文密码；错误密码始终落回 bcrypt。
type credentialVerifyCache struct {
	mu       sync.Mutex
	entries  map[[sha256.Size]byte]credentialCacheEntry
	ttl      time.Duration
	capacity int
	now      func() time.Time
	serial   uint64
}

func newCredentialVerifyCache(ttl time.Duration, capacity int, now func() time.Time) *credentialVerifyCache {
	if ttl <= 0 {
		ttl = defaultCredentialCacheTTL
	}
	if capacity <= 0 {
		capacity = defaultCredentialCacheCapacity
	}
	if now == nil {
		now = time.Now
	}
	return &credentialVerifyCache{
		entries:  make(map[[sha256.Size]byte]credentialCacheEntry),
		ttl:      ttl,
		capacity: capacity,
		now:      now,
	}
}

func credentialFingerprint(username, passwordHash, password string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, tokenHMACKey[:])
	var size [8]byte
	for _, value := range []string{username, passwordHash, password} {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(value))
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], mac.Sum(nil))
	return fingerprint
}

func (cache *credentialVerifyCache) hit(username, passwordHash, password string) bool {
	if cache == nil {
		return false
	}
	key := credentialFingerprint(username, passwordHash, password)
	now := cache.now()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok {
		return false
	}
	if !now.Before(entry.expiresAt) {
		delete(cache.entries, key)
		return false
	}
	cache.serial++
	entry.lastUsed = cache.serial
	cache.entries[key] = entry
	return true
}

func (cache *credentialVerifyCache) remember(username, passwordHash, password string) {
	if cache == nil {
		return
	}
	key := credentialFingerprint(username, passwordHash, password)
	now := cache.now()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneLocked(now)
	if len(cache.entries) >= cache.capacity {
		cache.evictLeastRecentlyUsedLocked()
	}
	cache.serial++
	cache.entries[key] = credentialCacheEntry{
		expiresAt: now.Add(cache.ttl),
		lastUsed:  cache.serial,
	}
}

func (cache *credentialVerifyCache) pruneLocked(now time.Time) {
	for key, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			delete(cache.entries, key)
		}
	}
}

func (cache *credentialVerifyCache) evictLeastRecentlyUsedLocked() {
	var oldestKey [sha256.Size]byte
	var oldestSerial uint64
	found := false
	for key, entry := range cache.entries {
		if !found || entry.lastUsed < oldestSerial {
			oldestKey = key
			oldestSerial = entry.lastUsed
			found = true
		}
	}
	if found {
		delete(cache.entries, oldestKey)
	}
}
