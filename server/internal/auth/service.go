// Package auth implements account registration, credential verification, and sessions.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

const (
	// SessionTTL is the lifetime required for Redis session:{token} entries.
	SessionTTL = 7 * 24 * time.Hour
)

// Service orchestrates durable accounts and Redis-backed sessions.
type Service struct {
	accounts store.AccountStore
	sessions store.SessionStore
}

// New constructs an Auth service from its persistence boundaries.
func New(accounts store.AccountStore, sessions store.SessionStore) *Service {
	return &Service{accounts: accounts, sessions: sessions}
}

// Register creates an account and returns the first seven-day session token.
func (s *Service) Register(ctx context.Context, username, password string) (uint64, string, error) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, "", fmt.Errorf("auth: hash password: %w", err)
	}

	uid, err := s.accounts.CreateAccount(ctx, username, string(passwordHash))
	if err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			return 0, "", pkgerr.UsernameTaken
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
			return 0, "", pkgerr.BadCredential
		}
		return 0, "", fmt.Errorf("auth: get account: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return 0, "", pkgerr.BadCredential
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
