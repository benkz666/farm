// Package auth implements account registration, credential verification, and sessions.
package auth

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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
	accounts store.FarmStore
	sessions store.SessionStore
}

// New constructs an Auth service from its persistence boundaries.
func New(accounts store.FarmStore, sessions store.SessionStore) *Service {
	return &Service{accounts: accounts, sessions: sessions}
}

// Register creates an account and returns the first seven-day session token.
func (s *Service) Register(ctx context.Context, username, password string) (uint64, string, error) {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, "", fmt.Errorf("auth: hash password: %w", err)
	}

	uid := nextUID()
	if err := s.accounts.SaveAccount(ctx, uid, username, string(passwordHash)); err != nil {
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

// Login verifies credentials and creates a new seven-day session token.
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

var uidSequence atomic.Uint64

func nextUID() uint64 {
	// 该格式只避免单进程内短时碰撞；多实例/时钟回拨下不可保证唯一，生产环境应换雪花或数据库 ID。
	return uint64(time.Now().UnixMilli())<<10 | uidSequence.Add(1)&0x3ff
}
