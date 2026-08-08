package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"farm/server/shared/telemetry"

	"github.com/redis/go-redis/v9"
)

const (
	mailLocalCacheTTL       = 45 * time.Second
	mailLocalCacheCapacity  = 8_192
	mailRedisCacheMinTTL    = 5 * time.Minute
	mailRedisCacheJitter    = 5 * time.Minute
	mailRedisVersionTTL     = 24 * time.Hour
	mailCacheStateShards    = 1_024
	mailInvalidationChannel = "mail:invalidation:v1"
)

var errMailboxInvalidated = errors.New("store: mailbox invalidated during read")

type mailboxCall struct {
	done  chan struct{}
	value []Mail
	err   error
}

type mailboxCacheState struct {
	mu      sync.Mutex
	version uint64
}

type mailboxCache struct {
	local   boundedTTLCache[uint64, []Mail]
	encoded boundedTTLCache[uint64, []byte]

	flightMu sync.Mutex
	flights  map[uint64]*mailboxCall
	state    [mailCacheStateShards]mailboxCacheState

	cancel    context.CancelFunc
	wait      sync.WaitGroup
	ready     chan struct{}
	readyOnce sync.Once
}

func cloneMails(mails []Mail) []Mail {
	if len(mails) == 0 {
		return []Mail{}
	}
	return append([]Mail(nil), mails...)
}

func mailboxStateIndex(uid uint64) uint64 {
	x := uid
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return (x ^ (x >> 31)) & (mailCacheStateShards - 1)
}

func (s *Store) mailboxGeneration(uid uint64) uint64 {
	state := &s.mailbox.state[mailboxStateIndex(uid)]
	state.mu.Lock()
	version := state.version
	state.mu.Unlock()
	return version
}

func (s *Store) deleteLocalMailbox(uid uint64) {
	if s == nil || uid == 0 {
		return
	}
	state := &s.mailbox.state[mailboxStateIndex(uid)]
	state.mu.Lock()
	state.version++
	s.mailbox.local.delete(uid)
	s.mailbox.encoded.delete(uid)
	state.mu.Unlock()
}

func (s *Store) putLocalMailboxIfCurrent(uid, generation uint64, mails []Mail) bool {
	state := &s.mailbox.state[mailboxStateIndex(uid)]
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.version != generation {
		return false
	}
	s.mailbox.local.put(uid, cloneMails(mails), time.Now())
	return true
}

func mailRedisVersionKey(uid uint64) string {
	return "mail:list:version:v1:" + strconv.FormatUint(uid, 10)
}

func mailRedisDataKey(uid, version uint64) string {
	return "mail:list:v1:" + strconv.FormatUint(uid, 10) + ":" + strconv.FormatUint(version, 10)
}

func mailRedisTTL(uid, version uint64) time.Duration {
	seconds := uint64(mailRedisCacheJitter / time.Second)
	if seconds == 0 {
		return mailRedisCacheMinTTL
	}
	return mailRedisCacheMinTTL + time.Duration((uid^version)%(seconds+1))*time.Second
}

func (s *Store) readMailboxVersion(ctx context.Context, uid uint64) (uint64, error) {
	value, err := s.rdb.Get(ctx, mailRedisVersionKey(uid)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	version, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: decode mailbox cache version: %w", err)
	}
	return version, nil
}

func (s *Store) loadMailboxCache(ctx context.Context, uid uint64) ([]Mail, bool, uint64, error) {
	if s == nil || s.rdb == nil {
		return nil, false, 0, nil
	}
	version, err := s.readMailboxVersion(ctx, uid)
	if err != nil {
		return nil, false, 0, err
	}
	encoded, err := s.rdb.Get(ctx, mailRedisDataKey(uid, version)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, version, nil
	}
	if err != nil {
		return nil, false, version, err
	}
	var mails []Mail
	if err := json.Unmarshal(encoded, &mails); err != nil {
		_ = s.rdb.Del(ctx, mailRedisDataKey(uid, version)).Err()
		return nil, false, version, fmt.Errorf("store: decode mailbox cache: %w", err)
	}
	return cloneMails(mails), true, version, nil
}

func (s *Store) writeMailboxCache(ctx context.Context, uid, version uint64, mails []Mail) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	if mails == nil {
		mails = []Mail{}
	}
	encoded, err := json.Marshal(mails)
	if err != nil {
		return fmt.Errorf("store: encode mailbox cache: %w", err)
	}
	return s.rdb.Set(ctx, mailRedisDataKey(uid, version), encoded, mailRedisTTL(uid, version)).Err()
}

func (s *Store) coalesceMailbox(ctx context.Context, uid uint64, load func() ([]Mail, error)) ([]Mail, error) {
	s.mailbox.flightMu.Lock()
	if s.mailbox.flights == nil {
		s.mailbox.flights = make(map[uint64]*mailboxCall)
	}
	if call := s.mailbox.flights[uid]; call != nil {
		s.mailbox.flightMu.Unlock()
		select {
		case <-call.done:
			return cloneMails(call.value), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &mailboxCall{done: make(chan struct{})}
	s.mailbox.flights[uid] = call
	s.mailbox.flightMu.Unlock()

	generation := s.mailboxGeneration(uid)
	value, err := load()
	if err == nil && !s.putLocalMailboxIfCurrent(uid, generation, value) {
		err = errMailboxInvalidated
		value = nil
	}

	s.mailbox.flightMu.Lock()
	call.value = cloneMails(value)
	call.err = err
	delete(s.mailbox.flights, uid)
	close(call.done)
	s.mailbox.flightMu.Unlock()
	return cloneMails(value), err
}

func (s *Store) invalidateMailboxAfterCommit(uid uint64) {
	if s == nil || uid == 0 {
		return
	}
	s.deleteLocalMailbox(uid)
	if s.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Incr(ctx, mailRedisVersionKey(uid))
		pipe.Expire(ctx, mailRedisVersionKey(uid), mailRedisVersionTTL)
		pipe.Publish(ctx, mailInvalidationChannel, strconv.FormatUint(uid, 10))
		return nil
	})
	if err != nil {
		telemetry.L().Warn("mailbox cache invalidation failed",
			"component", "store",
			"uid", uid,
			"err", err.Error(),
		)
	}
}

func (s *Store) startMailboxInvalidations() {
	if s == nil || s.rdb == nil || s.mailbox.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mailbox.cancel = cancel
	s.mailbox.ready = make(chan struct{})
	s.mailbox.wait.Add(1)
	go func() {
		defer s.mailbox.wait.Done()
		s.runMailboxInvalidations(ctx)
	}()
}

func (s *Store) stopMailboxInvalidations() {
	if s == nil || s.mailbox.cancel == nil {
		return
	}
	s.mailbox.cancel()
	s.mailbox.wait.Wait()
	s.mailbox.cancel = nil
}

func (s *Store) runMailboxInvalidations(ctx context.Context) {
	for ctx.Err() == nil {
		pubsub := s.rdb.Subscribe(ctx, mailInvalidationChannel)
		if _, err := pubsub.Receive(ctx); err != nil {
			_ = pubsub.Close()
			if !waitMailboxRetry(ctx) {
				return
			}
			continue
		}
		s.mailbox.readyOnce.Do(func() { close(s.mailbox.ready) })
		messages := pubsub.Channel(redis.WithChannelSize(1_024))
		for {
			select {
			case <-ctx.Done():
				_ = pubsub.Close()
				return
			case message, ok := <-messages:
				if !ok {
					_ = pubsub.Close()
					if !waitMailboxRetry(ctx) {
						return
					}
					goto reconnect
				}
				uid, err := strconv.ParseUint(message.Payload, 10, 64)
				if err == nil && uid != 0 {
					s.deleteLocalMailbox(uid)
				}
			}
		}
	reconnect:
	}
}

func waitMailboxRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
