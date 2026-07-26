package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

func TestRegisterDuplicateUsername(t *testing.T) {
	t.Parallel()

	accounts := newMemoryFarmStore()
	service := New(accounts, &memorySessionStore{})
	ctx := context.Background()

	if _, _, err := service.Register(ctx, "alice", "password-1"); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	_, _, err := service.Register(ctx, "alice", "password-2")
	if !errors.Is(err, pkgerr.UsernameTaken) {
		t.Fatalf("second Register error = %v, want code %d", err, pkgerr.UsernameTaken)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	t.Parallel()

	service := New(newMemoryFarmStore(), &memorySessionStore{})
	ctx := context.Background()
	if _, _, err := service.Register(ctx, "alice", "password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, _, err := service.Login(ctx, "alice", "wrong-password")
	if !errors.Is(err, pkgerr.BadCredential) {
		t.Fatalf("Login error = %v, want code %d", err, pkgerr.BadCredential)
	}
}

func TestLoginCreatesSevenDaySession(t *testing.T) {
	t.Parallel()

	sessions := &memorySessionStore{}
	service := New(newMemoryFarmStore(), sessions)
	ctx := context.Background()
	uid, _, err := service.Register(ctx, "alice", "password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	gotUID, token, err := service.Login(ctx, "alice", "password-1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotUID != uid {
		t.Fatalf("Login uid = %d, want %d", gotUID, uid)
	}
	if token == "" {
		t.Fatal("Login token is empty")
	}
	if sessions.token != token || sessions.uid != uid || sessions.ttl != SessionTTL {
		t.Fatalf("session = (%q, %d, %v), want (%q, %d, %v)",
			sessions.token, sessions.uid, sessions.ttl, token, uid, SessionTTL)
	}
}

type memoryFarmStore struct {
	accounts map[string]memoryAccount
}

type memoryAccount struct {
	uid          uint64
	passwordHash string
}

func newMemoryFarmStore() *memoryFarmStore {
	return &memoryFarmStore{accounts: make(map[string]memoryAccount)}
}

func (s *memoryFarmStore) SaveAccount(_ context.Context, uid uint64, username, passwordHash string) error {
	if _, exists := s.accounts[username]; exists {
		return store.ErrUsernameTaken
	}
	s.accounts[username] = memoryAccount{uid: uid, passwordHash: passwordHash}
	return nil
}

func (s *memoryFarmStore) GetAccountByUsername(_ context.Context, username string) (uint64, string, error) {
	account, exists := s.accounts[username]
	if !exists {
		return 0, "", store.ErrAccountNotFound
	}
	return account.uid, account.passwordHash, nil
}

func (*memoryFarmStore) LoadFarm(context.Context, uint64) (*farm.Aggregate, error) {
	return nil, store.ErrFarmNotFound
}

func (*memoryFarmStore) SaveFarm(context.Context, *farm.Aggregate) error {
	return nil
}

type memorySessionStore struct {
	token string
	uid   uint64
	ttl   time.Duration
}

func (s *memorySessionStore) Put(_ context.Context, token string, uid uint64, ttl time.Duration) error {
	s.token = token
	s.uid = uid
	s.ttl = ttl
	return nil
}

func (*memorySessionStore) Get(context.Context, string) (uint64, error) {
	return 0, store.ErrSessionNotFound
}

func (*memorySessionStore) Delete(context.Context, string) error {
	return nil
}

var (
	_ store.FarmStore    = (*memoryFarmStore)(nil)
	_ store.SessionStore = (*memorySessionStore)(nil)
)
