package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"farm/server/domain/farm"
	"farm/server/shared/errcode"
	"farm/server/shared/store"
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
	if !errors.Is(err, errcode.UsernameTaken) {
		t.Fatalf("second Register error = %v, want code %d", err, errcode.UsernameTaken)
	}
}

func TestRegisterUsesUIDAllocatedByAccountStore(t *testing.T) {
	accounts := newMemoryFarmStore()
	accounts.nextUID = 9_001
	service := New(accounts, &memorySessionStore{})

	uid, _, err := service.Register(context.Background(), "allocated", "password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if uid != 9_001 {
		t.Fatalf("Register uid = %d, want store-allocated 9001", uid)
	}
}

func TestRegisterUsesMinimumBcryptCost(t *testing.T) {
	accounts := newMemoryFarmStore()
	service := New(accounts, &memorySessionStore{})

	if _, _, err := service.Register(context.Background(), "minimum-cost", "password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, passwordHash, err := accounts.GetAccountByUsername(context.Background(), "minimum-cost")
	if err != nil {
		t.Fatalf("GetAccountByUsername: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(passwordHash))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	if cost != bcrypt.MinCost {
		t.Fatalf("bcrypt cost = %d, want minimum %d", cost, bcrypt.MinCost)
	}
}

func TestLoginMigratesExistingBcryptHashToMinimumCost(t *testing.T) {
	accounts := newMemoryFarmStore()
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	if err := accounts.SaveAccount(context.Background(), 7, "alice", string(passwordHash)); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	service := New(accounts, &memorySessionStore{})
	if _, _, err := service.Login(context.Background(), "alice", "secret"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, migratedHash, err := accounts.GetAccountByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetAccountByUsername: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(migratedHash))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	if cost != bcrypt.MinCost {
		t.Fatalf("migrated bcrypt cost = %d, want minimum %d", cost, bcrypt.MinCost)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(migratedHash), []byte("secret")); err != nil {
		t.Fatalf("migrated hash no longer verifies password: %v", err)
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
	if !errors.Is(err, errcode.BadCredential) {
		t.Fatalf("Login error = %v, want code %d", err, errcode.BadCredential)
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

func TestLoginCachesOnlySuccessfulExactCredentialVerification(t *testing.T) {
	accounts := newMemoryFarmStore()
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	if err := accounts.SaveAccount(context.Background(), 7, "alice", string(passwordHash)); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	service := New(accounts, &memorySessionStore{})
	compareCalls := 0
	service.comparePassword = func(_ []byte, password []byte) error {
		compareCalls++
		if string(password) != "secret" {
			return errors.New("password mismatch")
		}
		return nil
	}

	if _, _, err := service.Login(context.Background(), "alice", "secret"); err != nil {
		t.Fatalf("first Login: %v", err)
	}
	if _, _, err := service.Login(context.Background(), "alice", "secret"); err != nil {
		t.Fatalf("cached Login: %v", err)
	}
	if compareCalls != 1 {
		t.Fatalf("successful repeated credential compare calls = %d, want 1", compareCalls)
	}

	if _, _, err := service.Login(context.Background(), "alice", "wrong"); !errors.Is(err, errcode.BadCredential) {
		t.Fatalf("wrong password error = %v, want code %d", err, errcode.BadCredential)
	}
	if _, _, err := service.Login(context.Background(), "alice", "wrong"); !errors.Is(err, errcode.BadCredential) {
		t.Fatalf("repeated wrong password error = %v, want code %d", err, errcode.BadCredential)
	}
	if compareCalls != 3 {
		t.Fatalf("wrong password must always compare; calls = %d, want 3", compareCalls)
	}
}

type memoryFarmStore struct {
	accounts map[string]memoryAccount
	nextUID  uint64
}

type memoryAccount struct {
	uid          uint64
	passwordHash string
}

func newMemoryFarmStore() *memoryFarmStore {
	return &memoryFarmStore{accounts: make(map[string]memoryAccount)}
}

func (s *memoryFarmStore) CreateAccount(ctx context.Context, username, passwordHash string) (uint64, error) {
	if s.nextUID == 0 {
		s.nextUID = uint64(len(s.accounts) + 1)
	}
	uid := s.nextUID
	s.nextUID++
	if err := s.SaveAccount(ctx, uid, username, passwordHash); err != nil {
		return 0, err
	}
	return uid, nil
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

func (s *memoryFarmStore) UpdatePasswordHash(_ context.Context, uid uint64, previousHash, passwordHash string) (bool, error) {
	for username, account := range s.accounts {
		if account.uid != uid || account.passwordHash != previousHash {
			continue
		}
		account.passwordHash = passwordHash
		s.accounts[username] = account
		return true, nil
	}
	return false, nil
}

func (*memoryFarmStore) LoadFarm(context.Context, uint64) (*farm.Aggregate, error) {
	return nil, store.ErrFarmNotFound
}

func (*memoryFarmStore) SaveFarm(context.Context, *farm.Aggregate) error {
	return nil
}

func (*memoryFarmStore) SaveFarms(context.Context, []*farm.Aggregate) error {
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
	_ store.AccountStore = (*memoryFarmStore)(nil)
	_ store.FarmStore    = (*memoryFarmStore)(nil)
	_ store.SessionStore = (*memorySessionStore)(nil)
)
