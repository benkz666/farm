package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	farmv1 "farm/server/gen/farm/v1"
)

// materializeTaskClaims projects Actor-authoritative claim facts. Reward
// economy is intentionally absent: the enclosing Farm mutation already stores
// the Actor's absolute coin/exp value and FarmSeq.
func (s *Store) materializeTaskClaims(ctx context.Context, events []journalTaskClaimProjection) error {
	type key struct {
		uid    uint64
		dayKey int64
		taskID uint32
	}
	latest := make(map[key]journalTaskClaimProjection, len(events))
	for _, event := range events {
		itemKey := key{uid: event.uid, dayKey: event.dayKey, taskID: event.taskID}
		current, ok := latest[itemKey]
		if !ok || journalStreamAfter(event.streamMS, event.streamSeq, current.streamMS, current.streamSeq) {
			latest[itemKey] = event
		}
	}
	keys := make([]key, 0, len(latest))
	for itemKey := range latest {
		keys = append(keys, itemKey)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].uid != keys[j].uid {
			return keys[i].uid < keys[j].uid
		}
		if keys[i].dayKey != keys[j].dayKey {
			return keys[i].dayKey < keys[j].dayKey
		}
		return keys[i].taskID < keys[j].taskID
	})
	values := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*9)
	valid := keys[:0]
	for _, itemKey := range keys {
		definition, ok := dailyTaskDefinitionByID(dailyTaskDefinitionsFor(itemKey.uid, itemKey.dayKey), itemKey.taskID)
		if !ok {
			continue
		}
		event := latest[itemKey]
		values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, itemKey.uid, itemKey.dayKey, itemKey.taskID,
			definition.target, definition.target, definition.rewardCoin, event.claimedAt,
			event.streamMS, event.streamSeq)
		valid = append(valid, itemKey)
	}
	if len(values) == 0 {
		return nil
	}
	newer := `(VALUES(journal_stream_ms) > journal_stream_ms OR
		(VALUES(journal_stream_ms) = journal_stream_ms AND VALUES(journal_stream_seq) > journal_stream_seq))`
	query := `INSERT INTO player_task (
		uid, logic_day, task_id, progress, target, reward_coin, claimed_at,
		journal_stream_ms, journal_stream_seq
	) VALUES ` + strings.Join(values, ",") + `
	ON DUPLICATE KEY UPDATE
		progress = GREATEST(progress, VALUES(progress)),
		target = VALUES(target), reward_coin = VALUES(reward_coin),
		claimed_at = COALESCE(claimed_at, VALUES(claimed_at)),
		journal_stream_ms = IF(` + newer + `, VALUES(journal_stream_ms), journal_stream_ms),
		journal_stream_seq = IF(` + newer + `, VALUES(journal_stream_seq), journal_stream_seq)`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: materialize task claims: %w", err)
	}
	for _, itemKey := range valid {
		s.invalidateTaskCache(taskReadKey{uid: itemKey.uid, dayKey: itemKey.dayKey})
	}
	return nil
}

func (s *Store) materializeMailMutations(ctx context.Context, events []journalMailMutationProjection) error {
	type key struct {
		uid, mailID uint64
	}
	type state struct {
		readAt, claimedAt int64
		deleted           bool
	}
	states := make(map[key]state, len(events))
	for _, event := range events {
		itemKey := key{uid: event.uid, mailID: event.mailID}
		current := states[itemKey]
		switch event.kind {
		case farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_READ:
			current.readAt = max(current.readAt, event.occurredAt)
		case farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_CLAIM:
			current.claimedAt = max(current.claimedAt, event.occurredAt)
			current.readAt = max(current.readAt, event.occurredAt)
		case farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_DELETE:
			current.deleted = true
		default:
			return fmt.Errorf("store: unsupported async mail mutation kind %d", event.kind)
		}
		states[itemKey] = current
	}
	keys := make([]key, 0, len(states))
	for itemKey := range states {
		keys = append(keys, itemKey)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].uid < keys[j].uid || keys[i].uid == keys[j].uid && keys[i].mailID < keys[j].mailID
	})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin async mail projection: %w", err)
	}
	defer tx.Rollback()
	for _, mode := range []string{"read", "claim", "delete"} {
		selects := make([]string, 0, len(keys))
		args := make([]any, 0, len(keys)*3)
		for _, itemKey := range keys {
			value := states[itemKey]
			occurredAt := value.readAt
			include := occurredAt > 0
			if mode == "claim" {
				occurredAt, include = value.claimedAt, value.claimedAt > 0
			} else if mode == "delete" {
				include = value.deleted
			}
			if !include {
				continue
			}
			prefix := "SELECT ? AS uid, ? AS mail_id"
			if len(selects) > 0 {
				prefix = "SELECT ?, ?"
			}
			if mode != "delete" {
				prefix += ", ? AS occurred_at"
				args = append(args, itemKey.uid, itemKey.mailID, occurredAt)
			} else {
				args = append(args, itemKey.uid, itemKey.mailID)
			}
			selects = append(selects, prefix)
		}
		if len(selects) == 0 {
			continue
		}
		derived := strings.Join(selects, " UNION ALL ")
		var query string
		switch mode {
		case "read":
			query = `UPDATE mail AS m JOIN (` + derived + `) AS v ON v.uid=m.uid AND v.mail_id=m.id
				SET m.read_at=COALESCE(m.read_at, v.occurred_at)`
		case "claim":
			query = `UPDATE mail AS m JOIN (` + derived + `) AS v ON v.uid=m.uid AND v.mail_id=m.id
				SET m.claimed_at=COALESCE(m.claimed_at, v.occurred_at),
					m.read_at=COALESCE(m.read_at, v.occurred_at)`
		case "delete":
			query = `DELETE m FROM mail AS m JOIN (` + derived + `) AS v ON v.uid=m.uid AND v.mail_id=m.id`
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("store: materialize async mail %s: %w", mode, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit async mail projection: %w", err)
	}
	seenUID := make(map[uint64]struct{})
	for _, itemKey := range keys {
		if _, ok := seenUID[itemKey.uid]; ok {
			continue
		}
		seenUID[itemKey.uid] = struct{}{}
		s.invalidateMailboxAfterCommit(itemKey.uid)
	}
	return nil
}
