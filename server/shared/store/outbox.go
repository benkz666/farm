package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"farm/server/shared/outbox"
)

const (
	outboxClaimLease = 30 * time.Second
	outboxBatchLimit = 64
	// Direct gRPC settlement normally acks within tens of milliseconds. Delay
	// fallback claiming so the dispatcher does not duplicate every healthy
	// request; a crashed Gateway still recovers within this bounded window.
	outboxInitialDelay = 500 * time.Millisecond
)

// OutboxStore manages durable fan-out rows in farm_outbox.
type OutboxStore interface {
	InsertOutboxEvents(ctx context.Context, events []outbox.Event) error
	ClaimDueOutbox(ctx context.Context, limit int, now int64) ([]OutboxRow, error)
	MarkOutboxPublished(ctx context.Context, eventID string) error
	MarkOutboxRetry(ctx context.Context, eventID string, attempts int, nextAttemptAt int64) error
	MarkOutboxDeadLetter(ctx context.Context, eventID string, attempts int) error
	DeletePublishedOutboxBefore(ctx context.Context, before int64) (int64, error)
}

// OutboxRow is one claimed outbox entry ready for delivery.
type OutboxRow struct {
	EventID     string
	ProducerUID uint64
	TargetUID   uint64
	Kind        outbox.Kind
	Payload     []byte
	Attempts    int
}

func (s *Store) InsertOutboxEvents(ctx context.Context, events []outbox.Event) error {
	if s == nil || s.db == nil || len(events) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	nextAttemptAt := now + outboxInitialDelay.Milliseconds()
	values := make([]string, 0, len(events))
	args := make([]any, 0, len(events)*7)
	for _, event := range events {
		if event.EventID == "" || event.TargetUID == 0 || event.Kind == "" || len(event.Payload) == 0 {
			return errors.New("store: invalid outbox event")
		}
		values = append(values, "(?, ?, ?, ?, ?, 0, ?, NULL, ?)")
		args = append(args,
			event.EventID,
			event.ProducerUID,
			event.TargetUID,
			string(event.Kind),
			event.Payload,
			nextAttemptAt,
			now,
		)
	}
	query := `INSERT INTO farm_outbox (
		event_id, producer_uid, target_uid, kind, payload,
		attempts, next_attempt_at, published_at, created_at
	) VALUES ` + strings.Join(values, ",") + `
	ON DUPLICATE KEY UPDATE event_id = event_id`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: insert outbox events: %w", err)
	}
	return nil
}

func (s *Store) ClaimDueOutbox(ctx context.Context, limit int, now int64) ([]OutboxRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: nil store")
	}
	if limit <= 0 {
		limit = outboxBatchLimit
	}
	token, err := newClaimToken()
	if err != nil {
		return nil, fmt.Errorf("store: new claim token: %w", err)
	}
	claimUntil := now + outboxClaimLease.Milliseconds()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin claim outbox tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT event_id, producer_uid, target_uid, kind, payload, attempts
		FROM farm_outbox
		WHERE published_at IS NULL
		  AND dead_lettered_at IS NULL
		  AND next_attempt_at <= ?
		  AND (claim_until IS NULL OR claim_until <= ?)
		ORDER BY next_attempt_at ASC, created_at ASC
		LIMIT ?
		FOR UPDATE SKIP LOCKED`, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: select due outbox: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0, limit)
	claimed := make([]OutboxRow, 0, limit)
	for rows.Next() {
		var row OutboxRow
		var kind string
		if err := rows.Scan(&row.EventID, &row.ProducerUID, &row.TargetUID, &kind, &row.Payload, &row.Attempts); err != nil {
			return nil, fmt.Errorf("store: scan outbox row: %w", err)
		}
		row.Kind = outbox.Kind(kind)
		ids = append(ids, row.EventID)
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate outbox rows: %w", err)
	}
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: commit empty outbox claim: %w", err)
		}
		return nil, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, token, claimUntil)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	update := `UPDATE farm_outbox
		SET claim_token = ?, claim_until = ?
		WHERE event_id IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := tx.ExecContext(ctx, update, args...); err != nil {
		return nil, fmt.Errorf("store: claim outbox rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit outbox claim: %w", err)
	}
	return claimed, nil
}

func (s *Store) MarkOutboxPublished(ctx context.Context, eventID string) error {
	if s == nil || s.db == nil || eventID == "" {
		return errors.New("store: invalid mark published")
	}
	now := time.Now().UnixMilli()
	result, err := s.db.ExecContext(ctx, `
		UPDATE farm_outbox
		SET published_at = ?, dead_lettered_at = NULL, claim_token = NULL, claim_until = NULL
		WHERE event_id = ? AND published_at IS NULL`, now, eventID)
	if err != nil {
		return fmt.Errorf("store: mark outbox published: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) MarkOutboxRetry(ctx context.Context, eventID string, attempts int, nextAttemptAt int64) error {
	if s == nil || s.db == nil || eventID == "" {
		return errors.New("store: invalid mark retry")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE farm_outbox
		SET attempts = ?, next_attempt_at = ?, claim_token = NULL, claim_until = NULL
		WHERE event_id = ? AND published_at IS NULL`,
		attempts, nextAttemptAt, eventID)
	if err != nil {
		return fmt.Errorf("store: mark outbox retry: %w", err)
	}
	return nil
}

func (s *Store) MarkOutboxDeadLetter(ctx context.Context, eventID string, attempts int) error {
	if s == nil || s.db == nil || eventID == "" {
		return errors.New("store: invalid mark dead letter")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE farm_outbox
		SET attempts = ?, dead_lettered_at = ?, claim_token = NULL, claim_until = NULL
		WHERE event_id = ? AND published_at IS NULL`,
		attempts, time.Now().UnixMilli(), eventID)
	if err != nil {
		return fmt.Errorf("store: mark outbox dead letter: %w", err)
	}
	return nil
}

func (s *Store) DeletePublishedOutboxBefore(ctx context.Context, before int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store: nil store")
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM farm_outbox
		WHERE published_at IS NOT NULL AND published_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete published outbox: %w", err)
	}
	return result.RowsAffected()
}

func newClaimToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
