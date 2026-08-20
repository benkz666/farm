package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"farm/server/shared/gameconfig"
)

const (
	taskMailReadBatchWindow  = 2 * time.Millisecond
	mailByIDReadBatchWindow  = 2 * time.Millisecond
	taskMailReadBatchMax     = 128
	taskMailReadBatchTimeout = 3 * time.Second
)

type persistedTask struct {
	progress uint32
	claimed  bool
}

type taskReadSnapshot struct {
	byID               map[uint32]persistedTask
	legacyDailyClaimed bool
}

type taskReadBatchResult struct {
	snapshot taskReadSnapshot
	err      error
}

type taskReadBatchRequest struct {
	uid    uint64
	result chan taskReadBatchResult
}

type taskReadBatch struct {
	dayKey   int64
	requests []taskReadBatchRequest
	flush    sync.Once
}

type taskReadBatcher struct {
	mu      sync.Mutex
	batches map[int64]*taskReadBatch
}

type mailReadBatchResult struct {
	mails []Mail
	err   error
}

type mailByIDBatchResult struct {
	mail Mail
	err  error
}

type mailReadBatchRequest struct {
	uid    uint64
	result chan mailReadBatchResult
}

type mailReadBatch struct {
	requests []mailReadBatchRequest
	flush    sync.Once
}

type mailReadBatcher struct {
	mu        sync.Mutex
	batch     *mailReadBatch
	byIDBatch *mailByIDBatch
}

type mailByIDBatchRequest struct {
	uid, mailID uint64
	result      chan mailByIDBatchResult
}

type mailByIDBatch struct {
	requests []mailByIDBatchRequest
	flush    sync.Once
}

// loadTaskSnapshot folds concurrent cold Actor task reads into two bounded
// MySQL queries (player_task and legacy daily_login). A two millisecond window
// is short relative to the API SLO, but replaces thousands of one-UID queries
// with batches while preserving an independent result for every Actor.
func (s *Store) loadTaskSnapshot(ctx context.Context, uid uint64, dayKey int64) (taskReadSnapshot, error) {
	request := taskReadBatchRequest{uid: uid, result: make(chan taskReadBatchResult, 1)}
	s.taskBatches.mu.Lock()
	if s.taskBatches.batches == nil {
		s.taskBatches.batches = make(map[int64]*taskReadBatch)
	}
	batch := s.taskBatches.batches[dayKey]
	if batch == nil {
		batch = &taskReadBatch{dayKey: dayKey, requests: make([]taskReadBatchRequest, 0, taskMailReadBatchMax)}
		s.taskBatches.batches[dayKey] = batch
		time.AfterFunc(taskMailReadBatchWindow, func() { s.flushTaskReadBatch(batch) })
	}
	batch.requests = append(batch.requests, request)
	full := len(batch.requests) >= taskMailReadBatchMax
	if full && s.taskBatches.batches[dayKey] == batch {
		delete(s.taskBatches.batches, dayKey)
	}
	s.taskBatches.mu.Unlock()
	if full {
		go s.flushTaskReadBatch(batch)
	}

	select {
	case result := <-request.result:
		return result.snapshot, result.err
	case <-ctx.Done():
		return taskReadSnapshot{}, ctx.Err()
	}
}

func (s *Store) flushTaskReadBatch(batch *taskReadBatch) {
	batch.flush.Do(func() {
		s.taskBatches.mu.Lock()
		if s.taskBatches.batches[batch.dayKey] == batch {
			delete(s.taskBatches.batches, batch.dayKey)
		}
		requests := append([]taskReadBatchRequest(nil), batch.requests...)
		s.taskBatches.mu.Unlock()

		uids := uniqueTaskReadUIDs(requests)
		queryCtx, cancel := context.WithTimeout(context.Background(), taskMailReadBatchTimeout)
		snapshots, err := s.loadTaskSnapshotsBatch(queryCtx, batch.dayKey, uids)
		cancel()
		for _, request := range requests {
			request.result <- taskReadBatchResult{snapshot: snapshots[request.uid], err: err}
		}
	})
}

func (s *Store) loadTaskSnapshotsBatch(ctx context.Context, dayKey int64, uids []uint64) (map[uint64]taskReadSnapshot, error) {
	result := make(map[uint64]taskReadSnapshot, len(uids))
	if len(uids) == 0 {
		return result, nil
	}
	for _, uid := range uids {
		result[uid] = taskReadSnapshot{byID: make(map[uint32]persistedTask)}
	}

	args := make([]any, 0, len(uids)+1)
	args = append(args, dayKey)
	for _, uid := range uids {
		args = append(args, uid)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT uid, task_id, progress, claimed_at IS NOT NULL
		FROM player_task
		WHERE logic_day = ? AND uid IN (`+sqlPlaceholders(len(uids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: batch list tasks: %w", err)
	}
	for rows.Next() {
		var uid uint64
		var id, progress uint32
		var claimed bool
		if err := rows.Scan(&uid, &id, &progress, &claimed); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: scan batch task: %w", err)
		}
		snapshot := result[uid]
		snapshot.byID[id] = persistedTask{progress: progress, claimed: claimed}
		result[uid] = snapshot
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: iterate batch tasks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close batch task rows: %w", err)
	}

	startMs, nextStartMs, validDay := gameconfig.LocalDayBounds(dayKey)
	if !validDay {
		return result, nil
	}
	legacyArgs := make([]any, 0, len(uids)+2)
	for _, uid := range uids {
		legacyArgs = append(legacyArgs, uid)
	}
	legacyArgs = append(legacyArgs, startMs, nextStartMs)
	legacyRows, err := s.db.QueryContext(ctx, `
		SELECT uid
		FROM daily_login
		WHERE uid IN (`+sqlPlaceholders(len(uids))+`) AND created_at >= ? AND created_at < ?
		GROUP BY uid`, legacyArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: batch list legacy daily login: %w", err)
	}
	for legacyRows.Next() {
		var uid uint64
		if err := legacyRows.Scan(&uid); err != nil {
			_ = legacyRows.Close()
			return nil, fmt.Errorf("store: scan batch legacy daily login: %w", err)
		}
		snapshot := result[uid]
		snapshot.legacyDailyClaimed = true
		result[uid] = snapshot
	}
	if err := legacyRows.Err(); err != nil {
		_ = legacyRows.Close()
		return nil, fmt.Errorf("store: iterate batch legacy daily login: %w", err)
	}
	if err := legacyRows.Close(); err != nil {
		return nil, fmt.Errorf("store: close batch legacy daily rows: %w", err)
	}
	return result, nil
}

func (s *Store) listMailsFromMySQLBatched(ctx context.Context, uid uint64) ([]Mail, error) {
	request := mailReadBatchRequest{uid: uid, result: make(chan mailReadBatchResult, 1)}
	s.mailBatches.mu.Lock()
	batch := s.mailBatches.batch
	if batch == nil {
		batch = &mailReadBatch{requests: make([]mailReadBatchRequest, 0, taskMailReadBatchMax)}
		s.mailBatches.batch = batch
		time.AfterFunc(taskMailReadBatchWindow, func() { s.flushMailReadBatch(batch) })
	}
	batch.requests = append(batch.requests, request)
	full := len(batch.requests) >= taskMailReadBatchMax
	if full && s.mailBatches.batch == batch {
		s.mailBatches.batch = nil
	}
	s.mailBatches.mu.Unlock()
	if full {
		go s.flushMailReadBatch(batch)
	}

	select {
	case result := <-request.result:
		return cloneMails(result.mails), result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// GetMail loads only the requested durable mail. MailClaim does not need the
// rest of the mailbox, so concurrent cold Actor claims share one composite-key
// query instead of materializing every row for every UID.
func (s *Store) GetMail(ctx context.Context, uid, mailID uint64) (Mail, error) {
	request := mailByIDBatchRequest{
		uid: uid, mailID: mailID, result: make(chan mailByIDBatchResult, 1),
	}
	s.mailBatches.mu.Lock()
	batch := s.mailBatches.byIDBatch
	if batch == nil {
		batch = &mailByIDBatch{requests: make([]mailByIDBatchRequest, 0, taskMailReadBatchMax)}
		s.mailBatches.byIDBatch = batch
		time.AfterFunc(mailByIDReadBatchWindow, func() { s.flushMailByIDBatch(batch) })
	}
	batch.requests = append(batch.requests, request)
	full := len(batch.requests) >= taskMailReadBatchMax
	if full && s.mailBatches.byIDBatch == batch {
		s.mailBatches.byIDBatch = nil
	}
	s.mailBatches.mu.Unlock()
	if full {
		go s.flushMailByIDBatch(batch)
	}

	select {
	case result := <-request.result:
		return result.mail, result.err
	case <-ctx.Done():
		return Mail{}, ctx.Err()
	}
}

func (s *Store) flushMailByIDBatch(batch *mailByIDBatch) {
	batch.flush.Do(func() {
		s.mailBatches.mu.Lock()
		if s.mailBatches.byIDBatch == batch {
			s.mailBatches.byIDBatch = nil
		}
		requests := append([]mailByIDBatchRequest(nil), batch.requests...)
		s.mailBatches.mu.Unlock()

		queryCtx, cancel := context.WithTimeout(context.Background(), taskMailReadBatchTimeout)
		mails, err := s.loadMailsByIDBatch(queryCtx, requests)
		cancel()
		for _, request := range requests {
			mail, ok := mails[mailReadKey{uid: request.uid, mailID: request.mailID}]
			requestErr := err
			if requestErr == nil && !ok {
				requestErr = ErrMailNotFound
			}
			request.result <- mailByIDBatchResult{mail: mail, err: requestErr}
		}
	})
}

type mailReadKey struct {
	uid, mailID uint64
}

func (s *Store) loadMailsByIDBatch(
	ctx context.Context,
	requests []mailByIDBatchRequest,
) (map[mailReadKey]Mail, error) {
	result := make(map[mailReadKey]Mail, len(requests))
	if len(requests) == 0 {
		return result, nil
	}
	keys := make([]mailReadKey, 0, len(requests))
	seen := make(map[mailReadKey]struct{}, len(requests))
	for _, request := range requests {
		key := mailReadKey{uid: request.uid, mailID: request.mailID}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	args := make([]any, 0, len(keys)*2)
	tuples := make([]string, 0, len(keys))
	for _, key := range keys {
		tuples = append(tuples, "(?, ?)")
		args = append(args, key.uid, key.mailID)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT uid, id, title, attachment_coin, claimed_at IS NOT NULL,
		       read_at IS NOT NULL, created_at
		FROM mail
		WHERE (uid, id) IN (`+strings.Join(tuples, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: batch get mail: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid uint64
		var mail Mail
		if err := rows.Scan(&uid, &mail.ID, &mail.Title, &mail.AttachmentCoin, &mail.Claimed, &mail.Read, &mail.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan batch mail by id: %w", err)
		}
		result[mailReadKey{uid: uid, mailID: mail.ID}] = mail
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate batch mail by id: %w", err)
	}
	return result, nil
}

func (s *Store) flushMailReadBatch(batch *mailReadBatch) {
	batch.flush.Do(func() {
		s.mailBatches.mu.Lock()
		if s.mailBatches.batch == batch {
			s.mailBatches.batch = nil
		}
		requests := append([]mailReadBatchRequest(nil), batch.requests...)
		s.mailBatches.mu.Unlock()

		uids := uniqueMailReadUIDs(requests)
		queryCtx, cancel := context.WithTimeout(context.Background(), taskMailReadBatchTimeout)
		mails, err := s.loadMailsBatch(queryCtx, uids)
		cancel()
		for _, request := range requests {
			request.result <- mailReadBatchResult{mails: mails[request.uid], err: err}
		}
	})
}

func (s *Store) loadMailsBatch(ctx context.Context, uids []uint64) (map[uint64][]Mail, error) {
	result := make(map[uint64][]Mail, len(uids))
	if len(uids) == 0 {
		return result, nil
	}
	args := make([]any, 0, len(uids))
	for _, uid := range uids {
		args = append(args, uid)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT uid, id, title, attachment_coin, claimed_at IS NOT NULL,
		       read_at IS NOT NULL, created_at
		FROM mail
		WHERE uid IN (`+sqlPlaceholders(len(uids))+`)
		ORDER BY uid, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: batch list mails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid uint64
		var mail Mail
		if err := rows.Scan(&uid, &mail.ID, &mail.Title, &mail.AttachmentCoin, &mail.Claimed, &mail.Read, &mail.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan batch mail: %w", err)
		}
		result[uid] = append(result[uid], mail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate batch mails: %w", err)
	}
	return result, nil
}

func uniqueTaskReadUIDs(requests []taskReadBatchRequest) []uint64 {
	seen := make(map[uint64]struct{}, len(requests))
	result := make([]uint64, 0, len(requests))
	for _, request := range requests {
		if _, ok := seen[request.uid]; ok {
			continue
		}
		seen[request.uid] = struct{}{}
		result = append(result, request.uid)
	}
	return result
}

func uniqueMailReadUIDs(requests []mailReadBatchRequest) []uint64 {
	seen := make(map[uint64]struct{}, len(requests))
	result := make([]uint64, 0, len(requests))
	for _, request := range requests {
		if _, ok := seen[request.uid]; ok {
			continue
		}
		seen[request.uid] = struct{}{}
		result = append(result, request.uid)
	}
	return result
}

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}
