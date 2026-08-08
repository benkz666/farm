package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMailRedisTTLIsBoundedAndVersionedKeyChanges(t *testing.T) {
	for _, input := range [][2]uint64{{1, 0}, {42, 9}, {1<<63 + 7, 99}} {
		ttl := mailRedisTTL(input[0], input[1])
		if ttl < mailRedisCacheMinTTL || ttl > mailRedisCacheMinTTL+mailRedisCacheJitter {
			t.Fatalf("mailRedisTTL(%d,%d) = %v", input[0], input[1], ttl)
		}
	}
	if mailRedisDataKey(42, 1) == mailRedisDataKey(42, 2) {
		t.Fatal("mail cache data key does not include its version")
	}
}

func TestMailboxSingleflightCoalescesConcurrentLoad(t *testing.T) {
	storage := New(nil, nil, 0)
	call := &mailboxCall{done: make(chan struct{})}
	storage.mailbox.flights = map[uint64]*mailboxCall{42: call}
	var calls atomic.Int32
	load := func() ([]Mail, error) {
		calls.Add(1)
		return nil, errors.New("coalesced follower unexpectedly became leader")
	}

	const callers = 32
	start := make(chan struct{})
	calling := make(chan struct{}, callers)
	results := make(chan []Mail, callers)
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			calling <- struct{}{}
			mails, err := storage.coalesceMailbox(context.Background(), 42, load)
			results <- mails
			errs <- err
		}()
	}
	close(start)
	for range callers {
		<-calling
	}
	call.value = []Mail{{ID: 7, Title: "cached"}}
	close(call.done)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("coalesced load: %v", err)
		}
	}
	for mails := range results {
		if len(mails) != 1 || mails[0].ID != 7 {
			t.Fatalf("coalesced mails = %#v", mails)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("follower load calls = %d, want 0", calls.Load())
	}
}

func TestMailboxInvalidationRejectsInflightStaleFill(t *testing.T) {
	storage := New(nil, nil, 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := storage.coalesceMailbox(context.Background(), 99, func() ([]Mail, error) {
			close(entered)
			<-release
			return []Mail{{ID: 1, Title: "stale"}}, nil
		})
		result <- err
	}()
	<-entered
	storage.deleteLocalMailbox(99)
	close(release)
	if err := <-result; !errors.Is(err, errMailboxInvalidated) {
		t.Fatalf("in-flight result error = %v, want errMailboxInvalidated", err)
	}
	if mails, ok := storage.mailbox.local.get(99, time.Now()); ok {
		t.Fatalf("stale mailbox was cached: %#v", mails)
	}
}

func TestCloneMailsCachesEmptyMailboxWithoutNil(t *testing.T) {
	if mails := cloneMails(nil); mails == nil || len(mails) != 0 {
		t.Fatalf("cloneMails(nil) = %#v, want non-nil empty slice", mails)
	}
}

func TestDeleteLocalMailboxInvalidatesEncodedValue(t *testing.T) {
	storage := &Store{}
	storage.mailbox.encoded.put(99, []byte(`[]`), time.Now())
	storage.deleteLocalMailbox(99)
	if encoded, ok := storage.mailbox.encoded.get(99, time.Now()); ok {
		t.Fatalf("encoded mailbox survived invalidation: %s", encoded)
	}
}
