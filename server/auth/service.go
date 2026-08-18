// Package auth implements account registration, credential verification, and sessions.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"farm/server/shared/errcode"
	"farm/server/shared/store"
)

const (
	// SessionTTL is the lifetime required for Redis session:{token} entries.
	SessionTTL = 7 * 24 * time.Hour
	// PasswordHashCost is intentionally the minimum cost supported by bcrypt.
	// Existing hashes are migrated after the password has been verified.
	PasswordHashCost = bcrypt.MinCost
)

// Service orchestrates durable accounts and Redis-backed sessions.
type Service struct {
	accounts        store.AccountStore
	sessions        store.SessionStore
	credentials     *credentialVerifyCache
	comparePassword func(hashedPassword, password []byte) error
}

// New constructs an Auth service from its persistence boundaries.
func New(accounts store.AccountStore, sessions store.SessionStore) *Service {
	return &Service{
		accounts:        accounts,
		sessions:        sessions,
		credentials:     newCredentialVerifyCache(defaultCredentialCacheTTL, defaultCredentialCacheCapacity, time.Now),
		comparePassword: bcrypt.CompareHashAndPassword,
	}
}

// Register creates an account and returns the first seven-day session token.
func (s *Service) Register(ctx context.Context, username, password string) (uint64, string, error) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), PasswordHashCost)
	if err != nil {
		return 0, "", fmt.Errorf("auth: hash password: %w", err)
	}

	uid, err := s.accounts.CreateAccount(ctx, username, string(passwordHash))
	if err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			return 0, "", errcode.UsernameTaken
		}
		return 0, "", fmt.Errorf("auth: save account: %w", err)
	}

	token, err := issueToken()
	if err != nil {
		return 0, "", err
	}
	if err := s.sessions.Put(ctx, token, uid, SessionTTL); err != nil {
		return 0, "", fmt.Errorf("auth: create session: %w", err)
	}
	return uid, token, nil
}

// Login verifies credentials and atomically makes a new seven-day token the
// account's sole active session. A previous token remains distinguishable for
// a short grace period so its WebSocket can report ERR_KICKED.
func (s *Service) Login(ctx context.Context, username, password string) (uint64, string, error) {
	uid, passwordHash, err := s.accounts.GetAccountByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrAccountNotFound) {
			return 0, "", errcode.BadCredential
		}
		return 0, "", fmt.Errorf("auth: get account: %w", err)
	}
	if !s.credentials.hit(username, passwordHash, password) {
		if err := s.comparePassword([]byte(passwordHash), []byte(password)); err != nil {
			return 0, "", errcode.BadCredential
		}
	}
	passwordHash, err = s.migratePasswordHash(ctx, uid, passwordHash, password)
	if err != nil {
		return 0, "", err
	}
	s.credentials.remember(username, passwordHash, password)

	token, err := issueToken()
	if err != nil {
		return 0, "", err
	}
	if err := s.sessions.Put(ctx, token, uid, SessionTTL); err != nil {
		return 0, "", fmt.Errorf("auth: create session: %w", err)
	}
	return uid, token, nil
}

// migratePasswordHash makes the configured bcrypt cost effective for existing
// accounts. bcrypt stores its cost in each hash, so changing Register alone
// would leave old accounts paying the previous cost indefinitely.
func (s *Service) migratePasswordHash(ctx context.Context, uid uint64, previousHash, password string) (string, error) {
	cost, err := bcrypt.Cost([]byte(previousHash))
	if err != nil {
		return "", fmt.Errorf("auth: inspect password hash: %w", err)
	}
	if cost == PasswordHashCost {
		return previousHash, nil
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), PasswordHashCost)
	if err != nil {
		return "", fmt.Errorf("auth: migrate password hash: %w", err)
	}
	updated, err := s.accounts.UpdatePasswordHash(ctx, uid, previousHash, string(passwordHash))
	if err != nil {
		return "", fmt.Errorf("auth: save migrated password hash: %w", err)
	}
	if !updated {
		// Another request changed the hash after this login read it. Do not cache
		// a hash that may not be the database's current value.
		return previousHash, nil
	}
	return string(passwordHash), nil
}
