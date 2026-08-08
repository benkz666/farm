//go:build integration

package store

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
)

var mailboxIntegrationUID atomic.Uint64

func openMailboxIntegrationStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("FARM_MYSQL_DSN")
	if dsn == "" {
		dsn = "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"
	}
	addr := os.Getenv("FARM_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	storage, closeFn, err := Open(ctx, dsn, addr, time.Minute)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := closeFn(); err != nil {
			t.Logf("close store: %v", err)
		}
	})
	select {
	case <-storage.mailbox.ready:
	case <-ctx.Done():
		t.Fatalf("wait for mailbox invalidation subscriber: %v", ctx.Err())
	}
	return storage
}

func nextMailboxIntegrationUID() uint64 {
	return uint64(time.Now().UnixNano()) + mailboxIntegrationUID.Add(1)
}

func TestMailboxCacheEmptyCrossInstanceInvalidationAndMutations(t *testing.T) {
	reader := openMailboxIntegrationStore(t)
	writer := openMailboxIntegrationStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	uid := nextMailboxIntegrationUID()
	username := "mc_" + strconv.FormatUint(uid, 10)
	if len(username) > 32 {
		username = username[:32]
	}
	if err := writer.SaveAccount(ctx, uid, username, "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	// 空邮箱也必须进入两级缓存，避免无邮件用户持续打穿 MySQL。
	empty, err := reader.ListMails(ctx, uid)
	if err != nil {
		t.Fatalf("ListMails empty: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty mailbox = %#v, want non-nil empty slice", empty)
	}
	encoded, err := reader.rdb.Get(ctx, mailRedisDataKey(uid, 0)).Result()
	if err != nil {
		t.Fatalf("read empty Redis mailbox: %v", err)
	}
	if strings.TrimSpace(encoded) != "[]" {
		t.Fatalf("empty Redis mailbox = %q, want []", encoded)
	}
	if ttl, err := reader.rdb.TTL(ctx, mailRedisDataKey(uid, 0)).Result(); err != nil || ttl < mailRedisCacheMinTTL-time.Second || ttl > mailRedisCacheMinTTL+mailRedisCacheJitter {
		t.Fatalf("empty Redis mailbox TTL = %v, err=%v", ttl, err)
	}

	// 另一个实例写入邮件；版本递增 + Pub/Sub 应立即淘汰 reader 的进程缓存。
	issued, err := writer.IssueCodexRewards(ctx, uid, farm.CodexProgress{CropID: 1, HarvestCount: 50})
	if err != nil {
		t.Fatalf("IssueCodexRewards: %v", err)
	}
	if len(issued) != 3 {
		t.Fatalf("IssueCodexRewards issued %d rewards, want 3", len(issued))
	}
	eventually(t, 2*time.Second, func() bool {
		_, hit := reader.mailbox.local.get(uid, time.Now())
		return !hit
	}, "reader local mailbox was not invalidated by Redis Pub/Sub")

	mails, err := reader.ListMails(ctx, uid)
	if err != nil {
		t.Fatalf("ListMails after cross-instance write: %v", err)
	}
	if len(mails) != 3 {
		t.Fatalf("mailbox after cross-instance write = %#v, want 3 mails", mails)
	}
	version, err := reader.readMailboxVersion(ctx, uid)
	if err != nil || version != 1 {
		t.Fatalf("mailbox version after write = %d, err=%v, want 1", version, err)
	}

	if affected, err := reader.MarkMailsRead(ctx, uid, 0); err != nil || affected != 3 {
		t.Fatalf("MarkMailsRead affected=%d err=%v, want 3", affected, err)
	}
	mails, err = reader.ListMails(ctx, uid)
	if err != nil {
		t.Fatalf("ListMails after read: %v", err)
	}
	for _, mail := range mails {
		if !mail.Read {
			t.Fatalf("mail remains unread after cache invalidation: %#v", mail)
		}
	}

	claimed, err := reader.ClaimMail(ctx, uid, mails[0].ID)
	if err != nil {
		t.Fatalf("ClaimMail: %v", err)
	}
	if !claimed.Claimed || !claimed.Read {
		t.Fatalf("claimed mail = %#v", claimed)
	}
	mails, err = reader.ListMails(ctx, uid)
	if err != nil || len(mails) != 3 || !mails[0].Claimed {
		t.Fatalf("ListMails after claim = %#v, err=%v", mails, err)
	}

	if affected, err := reader.DeleteMails(ctx, uid, claimed.ID); err != nil || affected != 1 {
		t.Fatalf("DeleteMails affected=%d err=%v, want 1", affected, err)
	}
	mails, err = reader.ListMails(ctx, uid)
	if err != nil || len(mails) != 2 {
		t.Fatalf("ListMails after delete = %#v, err=%v, want 2 mails", mails, err)
	}
	version, err = reader.readMailboxVersion(ctx, uid)
	if err != nil || version != 4 {
		t.Fatalf("mailbox version after write/read/claim/delete = %d, err=%v, want 4", version, err)
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool, failure string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal(failure)
	}
}
