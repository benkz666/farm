package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/gameconfig"
	"farm/server/shared/outbox"
	"farm/server/shared/telemetry"
)

// SaveFarms 在一个 MySQL 事务内批量持久化多个农场聚合，成功后 best-effort 批量写 Redis。
func (s *Store) SaveFarms(ctx context.Context, snapshots []*farm.Aggregate) error {
	commits := make([]outbox.FarmCommit, len(snapshots))
	for i, snap := range snapshots {
		commits[i] = outbox.FarmCommit{Snapshot: snap}
	}
	return s.CommitFarms(ctx, commits)
}

// CommitFarms persists snapshots and outbox rows atomically.
func (s *Store) CommitFarms(ctx context.Context, commits []outbox.FarmCommit) error {
	if s == nil {
		return errors.New("store: nil store")
	}
	if len(commits) == 0 {
		return nil
	}
	fullSnapshots := make([]*farm.Aggregate, 0, len(commits))
	var fullOutbox []outbox.Event
	specialized := make([]outbox.FarmCommit, 0, len(commits))
	for _, commit := range commits {
		if commit.Snapshot == nil {
			return errors.New("store: invalid farm commit")
		}
		if commit.Plan.Mode == outbox.PersistFull ||
			(commit.Plan.Mode != outbox.PersistCrossOwner && len(commit.Outbox) > 0) {
			fullSnapshots = append(fullSnapshots, commit.Snapshot)
			fullOutbox = append(fullOutbox, commit.Outbox...)
			continue
		}
		specialized = append(specialized, commit)
	}
	if len(fullSnapshots) > 0 {
		if err := s.saveFarmsToMySQL(ctx, fullSnapshots, fullOutbox); err != nil {
			return err
		}
		if err := s.cacheFarmsPipeline(ctx, fullSnapshots); err != nil {
			logFarmCacheFailure("cache_farms_pipeline", fullSnapshots, err)
		}
	}
	if len(specialized) > 0 {
		if err := s.saveSpecializedFarmCommits(ctx, specialized); err != nil {
			return err
		}
		snapshots := make([]*farm.Aggregate, 0, len(specialized))
		for _, commit := range specialized {
			snapshots = append(snapshots, commit.Snapshot)
		}
		if err := s.cacheFarmsPipeline(ctx, snapshots); err != nil {
			logFarmCacheFailure("cache_specialized_farms_pipeline", snapshots, err)
			s.invalidateFarmCaches(snapshots)
		}
	}
	return nil
}

func logFarmCacheFailure(op string, snapshots []*farm.Aggregate, err error) {
	telemetry.L().Error("store farm cache pipeline failed",
		"component", "store", "op", op, "count", len(snapshots), "err", err.Error())
}

func (s *Store) invalidateFarmCaches(snapshots []*farm.Aggregate) {
	if s == nil || s.rdb == nil || len(snapshots) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pipe := s.rdb.Pipeline()
	for _, snapshot := range snapshots {
		if snapshot != nil && snapshot.UID != 0 {
			pipe.Del(ctx, farmKey(snapshot.UID))
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		telemetry.L().Error("store farm cache invalidation failed",
			"component", "store", "op", "invalidate_specialized_farms", "err", err.Error())
	}
}

// SaveEconomy persists the subset mutated by Buy/Sell. It deliberately avoids
// rewriting plots, daily/cross blobs and codex rows on every shop operation.
func (s *Store) SaveEconomy(ctx context.Context, agg *farm.Aggregate) error {
	return s.CommitFarms(ctx, []outbox.FarmCommit{{
		Snapshot: agg, Plan: outbox.PersistPlan{Mode: outbox.PersistEconomy},
	}})
}

// SaveCrossVisitor persists visitor reservation/settlement state without
// touching plots or codex. Settlement can also modify inventory and level.
func (s *Store) SaveCrossVisitor(ctx context.Context, agg *farm.Aggregate, includeItems bool) error {
	return s.CommitFarms(ctx, []outbox.FarmCommit{{
		Snapshot: agg,
		Plan:     outbox.PersistPlan{Mode: outbox.PersistCrossVisitor, IncludeItems: includeItems},
	}})
}

// CommitCrossOwner atomically persists one owner plot, owner-side state and
// the result outbox. This preserves the cross-farm durable boundary while
// avoiding a rewrite of all plots and inventory.
func (s *Store) CommitCrossOwner(ctx context.Context, agg *farm.Aggregate, plotIndex uint8, event outbox.Event) error {
	return s.CommitFarms(ctx, []outbox.FarmCommit{{
		Snapshot: agg, Outbox: []outbox.Event{event},
		Plan: outbox.PersistPlan{Mode: outbox.PersistCrossOwner, PlotIndex: plotIndex},
	}})
}

func encodeCrossVisitorBlobs(agg *farm.Aggregate) ([]byte, []byte, error) {
	dailyBlob, err := json.Marshal(agg.Daily)
	if err != nil {
		return nil, nil, fmt.Errorf("store: encode cross visitor daily uid %d: %w", agg.UID, err)
	}
	crossBlob := []byte{}
	if len(agg.CrossPending) > 0 {
		crossBlob, err = json.Marshal(agg.CrossPending)
		if err != nil {
			return nil, nil, fmt.Errorf("store: encode cross visitor pending uid %d: %w", agg.UID, err)
		}
	}
	return dailyBlob, crossBlob, nil
}

func (s *Store) saveSpecializedFarmCommits(ctx context.Context, commits []outbox.FarmCommit) error {
	if s == nil || s.db == nil || len(commits) == 0 {
		return errors.New("store: invalid specialized farm commits")
	}
	now := time.Now().UnixMilli()
	var economyRows, localPlotRows, visitorRows, ownerRows []string
	var economyArgs, localPlotArgs, visitorArgs, ownerArgs []any
	var itemUIDs []uint64
	var itemSnapshots []*farm.Aggregate
	var plotValues []string
	var plotArgs []any
	var outboxEvents []outbox.Event
	var codexValues []string
	var codexArgs []any

	for _, commit := range commits {
		agg := commit.Snapshot
		if agg == nil || agg.UID == 0 {
			return errors.New("store: invalid specialized farm snapshot")
		}
		switch commit.Plan.Mode {
		case outbox.PersistEconomy:
			petBlob, err := json.Marshal(agg.Pet)
			if err != nil {
				return fmt.Errorf("store: encode economy pet uid %d: %w", agg.UID, err)
			}
			row := "SELECT ?, ?, ?, ?, ?, ?, ?"
			if len(economyRows) == 0 {
				row = "SELECT ? AS uid, ? AS level_value, ? AS exp_value, ? AS coin, ? AS pet_blob, ? AS farm_seq, ? AS updated_at"
			}
			economyRows = append(economyRows, row)
			economyArgs = append(economyArgs, agg.UID, agg.Level, agg.Exp, agg.Coin, petBlob, agg.FarmSeq, now)
			itemUIDs = append(itemUIDs, agg.UID)
			itemSnapshots = append(itemSnapshots, agg)

		case outbox.PersistPlot:
			if int(commit.Plan.PlotIndex) >= len(agg.Plots) {
				return errors.New("store: invalid specialized local plot commit")
			}
			plotBlob, err := EncodePlot(agg.Plots[commit.Plan.PlotIndex])
			if err != nil {
				return fmt.Errorf("store: encode local plot uid %d: %w", agg.UID, err)
			}
			row := "SELECT ?, ?, ?, ?, CAST(? AS BINARY(8)), ?, ?"
			if len(localPlotRows) == 0 {
				row = "SELECT ? AS uid, ? AS level_value, ? AS exp_value, ? AS coin, " +
					"CAST(? AS BINARY(8)) AS codex_bitmap, ? AS farm_seq, ? AS updated_at"
			}
			localPlotRows = append(localPlotRows, row)
			localPlotArgs = append(localPlotArgs,
				agg.UID, agg.Level, agg.Exp, agg.Coin,
				encodeCodexBitmap(agg.CodexHarvests), agg.FarmSeq, now,
			)
			plotValues = append(plotValues, "(?, ?, ?)")
			plotArgs = append(plotArgs, agg.UID, commit.Plan.PlotIndex, plotBlob)
			if commit.Plan.IncludeItems {
				itemUIDs = append(itemUIDs, agg.UID)
				itemSnapshots = append(itemSnapshots, agg)
			}
			if commit.Plan.IncludeCodex {
				for cropID, count := range agg.CodexHarvests {
					if count == 0 {
						continue
					}
					if _, ok := gameconfig.CropByID(cropID); !ok {
						return fmt.Errorf("store: invalid codex crop ID %d", cropID)
					}
					codexValues = append(codexValues, "(?, ?, ?, ?)")
					codexArgs = append(codexArgs, agg.UID, cropID, count, now)
				}
			}

		case outbox.PersistCrossVisitor:
			dailyBlob, crossBlob, err := encodeCrossVisitorBlobs(agg)
			if err != nil {
				return err
			}
			row := "SELECT ?, ?, ?, ?, ?, ?, ?, ?"
			if len(visitorRows) == 0 {
				row = "SELECT ? AS uid, ? AS level_value, ? AS exp_value, ? AS coin, ? AS daily_blob, ? AS cross_blob, ? AS farm_seq, ? AS updated_at"
			}
			visitorRows = append(visitorRows, row)
			visitorArgs = append(visitorArgs, agg.UID, agg.Level, agg.Exp, agg.Coin, dailyBlob, crossBlob, agg.FarmSeq, now)
			if commit.Plan.IncludeItems {
				itemUIDs = append(itemUIDs, agg.UID)
				itemSnapshots = append(itemSnapshots, agg)
			}

		case outbox.PersistCrossOwner:
			if int(commit.Plan.PlotIndex) >= len(agg.Plots) || len(commit.Outbox) == 0 {
				return errors.New("store: invalid specialized cross owner commit")
			}
			petBlob, err := json.Marshal(agg.Pet)
			if err != nil {
				return fmt.Errorf("store: encode cross owner pet uid %d: %w", agg.UID, err)
			}
			receiptBlob := []byte{}
			if len(agg.CrossReceipts) > 0 {
				receiptBlob, err = json.Marshal(agg.CrossReceipts)
				if err != nil {
					return fmt.Errorf("store: encode cross receipts uid %d: %w", agg.UID, err)
				}
			}
			plotBlob, err := EncodePlot(agg.Plots[commit.Plan.PlotIndex])
			if err != nil {
				return fmt.Errorf("store: encode cross owner plot uid %d: %w", agg.UID, err)
			}
			row := "SELECT ?, ?, ?, ?, ?, ?, ?, ?"
			if len(ownerRows) == 0 {
				row = "SELECT ? AS uid, ? AS level_value, ? AS exp_value, ? AS coin, ? AS pet_blob, ? AS cross_receipt_blob, ? AS farm_seq, ? AS updated_at"
			}
			ownerRows = append(ownerRows, row)
			ownerArgs = append(ownerArgs, agg.UID, agg.Level, agg.Exp, agg.Coin, petBlob, receiptBlob, agg.FarmSeq, now)
			plotValues = append(plotValues, "(?, ?, ?)")
			plotArgs = append(plotArgs, agg.UID, commit.Plan.PlotIndex, plotBlob)
			outboxEvents = append(outboxEvents, commit.Outbox...)

		default:
			return fmt.Errorf("store: unsupported specialized persist mode %d", commit.Plan.Mode)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin specialized farm tx: %w", err)
	}
	defer tx.Rollback()

	if len(economyRows) > 0 {
		query := `UPDATE player AS p JOIN (` + strings.Join(economyRows, " UNION ALL ") + `) AS v ON p.uid = v.uid
			SET p.level = v.level_value, p.exp = v.exp_value, p.coin = v.coin,
				p.pet_blob = v.pet_blob, p.farm_seq = v.farm_seq, p.updated_at = v.updated_at`
		if _, err := tx.ExecContext(ctx, query, economyArgs...); err != nil {
			return fmt.Errorf("store: batch update economy: %w", err)
		}
	}
	if len(localPlotRows) > 0 {
		query := `UPDATE player AS p JOIN (` + strings.Join(localPlotRows, " UNION ALL ") + `) AS v ON p.uid = v.uid
			SET p.level = v.level_value, p.exp = v.exp_value, p.coin = v.coin,
				p.codex_bitmap = v.codex_bitmap,
				p.farm_seq = v.farm_seq, p.updated_at = v.updated_at`
		if _, err := tx.ExecContext(ctx, query, localPlotArgs...); err != nil {
			return fmt.Errorf("store: batch update local plots: %w", err)
		}
	}
	if len(visitorRows) > 0 {
		query := `UPDATE player AS p JOIN (` + strings.Join(visitorRows, " UNION ALL ") + `) AS v ON p.uid = v.uid
			SET p.level = v.level_value, p.exp = v.exp_value, p.coin = v.coin,
				p.daily_blob = v.daily_blob, p.cross_blob = v.cross_blob,
				p.farm_seq = v.farm_seq, p.updated_at = v.updated_at`
		if _, err := tx.ExecContext(ctx, query, visitorArgs...); err != nil {
			return fmt.Errorf("store: batch update cross visitors: %w", err)
		}
	}
	if len(ownerRows) > 0 {
		query := `UPDATE player AS p JOIN (` + strings.Join(ownerRows, " UNION ALL ") + `) AS v ON p.uid = v.uid
			SET p.level = v.level_value, p.exp = v.exp_value, p.coin = v.coin,
				p.pet_blob = v.pet_blob, p.cross_receipt_blob = v.cross_receipt_blob,
				p.farm_seq = v.farm_seq, p.updated_at = v.updated_at`
		if _, err := tx.ExecContext(ctx, query, ownerArgs...); err != nil {
			return fmt.Errorf("store: batch update cross owners: %w", err)
		}
	}
	if len(plotValues) > 0 {
		query := "INSERT INTO farm_plot (uid, plot_index, `blob`) VALUES " + strings.Join(plotValues, ",") +
			" ON DUPLICATE KEY UPDATE `blob` = VALUES(`blob`)"
		if _, err := tx.ExecContext(ctx, query, plotArgs...); err != nil {
			return fmt.Errorf("store: batch upsert selected plots: %w", err)
		}
	}
	if err := batchReplaceItemsTx(ctx, tx, itemUIDs, itemSnapshots); err != nil {
		return err
	}
	if len(codexValues) > 0 {
		query := `INSERT INTO player_codex (uid, crop_id, harvest_count, updated_at) VALUES ` +
			strings.Join(codexValues, ",") + `
			ON DUPLICATE KEY UPDATE
				harvest_count = GREATEST(harvest_count, VALUES(harvest_count)),
				updated_at = VALUES(updated_at)`
		if _, err := tx.ExecContext(ctx, query, codexArgs...); err != nil {
			return fmt.Errorf("store: batch upsert selected player_codex: %w", err)
		}
	}
	if err := insertOutboxEventsTx(ctx, tx, outboxEvents, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit specialized farms: %w", err)
	}
	return nil
}

func (s *Store) saveFarmsToMySQL(ctx context.Context, snapshots []*farm.Aggregate, outboxEvents []outbox.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin save farms tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()

	uids := make([]uint64, 0, len(snapshots))
	playerRows := make([]string, 0, len(snapshots))
	playerArgs := make([]any, 0, len(snapshots)*13)
	plotValues := make([]string, 0, len(snapshots)*gameconfig.MaxPlots)
	plotArgs := make([]any, 0, len(snapshots)*gameconfig.MaxPlots*3)
	codexValues := make([]string, 0)
	codexArgs := make([]any, 0)

	for _, agg := range snapshots {
		if agg == nil || agg.UID == 0 {
			return errors.New("store: invalid farm snapshot")
		}
		uids = append(uids, agg.UID)

		dailyBlob, err := json.Marshal(agg.Daily)
		if err != nil {
			return fmt.Errorf("store: encode daily state uid %d: %w", agg.UID, err)
		}
		petBlob, err := json.Marshal(agg.Pet)
		if err != nil {
			return fmt.Errorf("store: encode pet state uid %d: %w", agg.UID, err)
		}
		crossBlob := []byte{}
		if len(agg.CrossPending) > 0 {
			if crossBlob, err = json.Marshal(agg.CrossPending); err != nil {
				return fmt.Errorf("store: encode cross pending uid %d: %w", agg.UID, err)
			}
		}
		crossReceiptBlob := []byte{}
		if len(agg.CrossReceipts) > 0 {
			if crossReceiptBlob, err = json.Marshal(agg.CrossReceipts); err != nil {
				return fmt.Errorf("store: encode cross receipts uid %d: %w", agg.UID, err)
			}
		}

		row := "SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?"
		if len(playerRows) == 0 {
			row = "SELECT ? AS uid, ? AS nickname, ? AS level_value, ? AS exp_value, " +
				"? AS coin, ? AS unlocked_plots, CAST(? AS BINARY(8)) AS codex_bitmap, ? AS daily_blob, " +
				"? AS pet_blob, ? AS cross_blob, ? AS cross_receipt_blob, ? AS farm_seq, ? AS updated_at"
		}
		playerRows = append(playerRows, row)
		playerArgs = append(playerArgs,
			agg.UID, agg.Nickname, agg.Level, agg.Exp, agg.Coin, agg.UnlockedPlots,
			encodeCodexBitmap(agg.CodexHarvests), dailyBlob, petBlob, crossBlob,
			crossReceiptBlob, agg.FarmSeq, now,
		)

		for i := 0; i < gameconfig.MaxPlots; i++ {
			blob, err := EncodePlot(agg.Plots[i])
			if err != nil {
				return fmt.Errorf("store: encode plot %d uid %d: %w", i, agg.UID, err)
			}
			plotValues = append(plotValues, "(?, ?, ?)")
			plotArgs = append(plotArgs, agg.UID, i, blob)
		}

		for cropID, count := range agg.CodexHarvests {
			if count == 0 {
				continue
			}
			if _, ok := gameconfig.CropByID(cropID); !ok {
				return fmt.Errorf("store: invalid codex crop ID %d", cropID)
			}
			codexValues = append(codexValues, "(?, ?, ?, ?)")
			codexArgs = append(codexArgs, agg.UID, cropID, count, now)
		}
	}

	playerQuery := `UPDATE player AS p JOIN (` + strings.Join(playerRows, " UNION ALL ") + `) AS v
		ON p.uid = v.uid
		SET p.nickname = v.nickname,
			p.level = v.level_value,
			p.exp = v.exp_value,
			p.coin = v.coin,
			p.unlocked_plots = v.unlocked_plots,
			p.codex_bitmap = v.codex_bitmap,
			p.daily_blob = v.daily_blob,
			p.pet_blob = v.pet_blob,
			p.cross_blob = v.cross_blob,
			p.cross_receipt_blob = v.cross_receipt_blob,
			p.farm_seq = v.farm_seq,
			p.updated_at = v.updated_at`
	if _, err := tx.ExecContext(ctx, playerQuery, playerArgs...); err != nil {
		return fmt.Errorf("store: batch update player: %w", err)
	}

	if len(plotValues) > 0 {
		query := "INSERT INTO farm_plot (uid, plot_index, `blob`) VALUES " +
			strings.Join(plotValues, ",") +
			" ON DUPLICATE KEY UPDATE `blob` = VALUES(`blob`)"
		if _, err := tx.ExecContext(ctx, query, plotArgs...); err != nil {
			return fmt.Errorf("store: batch upsert farm_plot: %w", err)
		}
	}

	if err := batchReplaceItemsTx(ctx, tx, uids, snapshots); err != nil {
		return err
	}

	if len(codexValues) > 0 {
		query := `INSERT INTO player_codex (uid, crop_id, harvest_count, updated_at) VALUES ` +
			strings.Join(codexValues, ",") + `
			ON DUPLICATE KEY UPDATE
				harvest_count = GREATEST(harvest_count, VALUES(harvest_count)),
				updated_at = VALUES(updated_at)`
		if _, err := tx.ExecContext(ctx, query, codexArgs...); err != nil {
			return fmt.Errorf("store: batch upsert player_codex: %w", err)
		}
	}

	if err := insertOutboxEventsTx(ctx, tx, outboxEvents, now); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit save farms tx: %w", err)
	}
	return nil
}

func insertOutboxEventsTx(ctx context.Context, tx *sql.Tx, events []outbox.Event, now int64) error {
	if len(events) == 0 {
		return nil
	}
	values := make([]string, 0, len(events))
	args := make([]any, 0, len(events)*7)
	seen := make(map[string]struct{}, len(events))
	nextAttemptAt := now + outboxInitialDelay.Milliseconds()
	for _, event := range events {
		if event.EventID == "" || len(event.Payload) == 0 {
			return errors.New("store: invalid outbox event in batch")
		}
		if _, ok := seen[event.EventID]; ok {
			continue
		}
		seen[event.EventID] = struct{}{}
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
	if len(values) == 0 {
		return nil
	}
	query := `INSERT INTO farm_outbox (
		event_id, producer_uid, target_uid, kind, payload,
		attempts, next_attempt_at, published_at, created_at
	) VALUES ` + strings.Join(values, ",") + `
	ON DUPLICATE KEY UPDATE event_id = event_id`
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: batch insert outbox: %w", err)
	}
	return nil
}

func batchReplaceItemsTx(ctx context.Context, tx *sql.Tx, uids []uint64, snapshots []*farm.Aggregate) error {
	if len(uids) == 0 {
		return nil
	}
	placeholders := make([]string, len(uids))
	args := make([]any, len(uids))
	for i, uid := range uids {
		placeholders[i] = "?"
		args[i] = uid
	}
	deleteQuery := "DELETE FROM item WHERE uid IN (" + strings.Join(placeholders, ",") + ")"
	if _, err := tx.ExecContext(ctx, deleteQuery, args...); err != nil {
		return fmt.Errorf("store: batch delete item: %w", err)
	}

	insertValues := make([]string, 0)
	insertArgs := make([]any, 0)
	for _, agg := range snapshots {
		for key, count := range agg.Items {
			if count == 0 {
				continue
			}
			kind, itemID, err := ParseItemKey(key)
			if err != nil {
				return fmt.Errorf("store: parse item key %q uid %d: %w", key, agg.UID, err)
			}
			insertValues = append(insertValues, "(?, ?, ?, ?)")
			insertArgs = append(insertArgs, agg.UID, kind, itemID, count)
		}
	}
	if len(insertValues) == 0 {
		return nil
	}
	insertQuery := "INSERT INTO item (uid, kind, item_id, count) VALUES " + strings.Join(insertValues, ",")
	if _, err := tx.ExecContext(ctx, insertQuery, insertArgs...); err != nil {
		return fmt.Errorf("store: batch insert item: %w", err)
	}
	return nil
}

func (s *Store) cacheFarmsPipeline(ctx context.Context, snapshots []*farm.Aggregate) error {
	if s == nil || s.rdb == nil || len(snapshots) == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	for _, agg := range snapshots {
		if agg == nil {
			continue
		}
		payload, err := json.Marshal(agg)
		if err != nil {
			return fmt.Errorf("store: marshal farm cache uid %d: %w", agg.UID, err)
		}
		pipe.Set(ctx, farmKey(agg.UID), payload, s.farmTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("store: exec farm cache pipeline: %w", err)
	}
	return nil
}
