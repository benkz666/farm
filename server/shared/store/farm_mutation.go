package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/gameconfig"
	"farm/server/shared/outbox"
)

// MaterializeFarmMutations projects Protobuf increment records in one fixed
// table order: player -> farm_plot -> item -> player_codex -> farm_outbox.
// Every row collection is sorted by UID and its secondary key before SQL.
func (s *Store) MaterializeFarmMutations(ctx context.Context, mutations []*farmv1.FarmWriteMutation) error {
	if s == nil || s.db == nil || len(mutations) == 0 {
		return errors.New("store: invalid farm mutation batch")
	}
	mutations = append([]*farmv1.FarmWriteMutation(nil), mutations...)
	sort.Slice(mutations, func(left, right int) bool { return mutations[left].Uid < mutations[right].Uid })
	hasFarmRows, err := validateFarmMutations(mutations)
	if err != nil {
		return err
	}
	// Task/mail-only records deliberately carry no farm rows. Their dedicated
	// projector transaction below is the sole MySQL commit, avoiding an empty
	// transaction and a second fsync for MailRead/MailDelete.
	if !hasFarmRows {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin farm mutation tx: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()

	if err := materializeMutationPlayers(ctx, tx, mutations, now); err != nil {
		return err
	}
	if err := materializeMutationPlots(ctx, tx, mutations); err != nil {
		return err
	}
	if err := materializeMutationItems(ctx, tx, mutations); err != nil {
		return err
	}
	if err := materializeMutationCodex(ctx, tx, mutations, now); err != nil {
		return err
	}
	events := mutationOutboxEvents(mutations)
	if err := insertOutboxEventsTx(ctx, tx, s.outboxScope(), events, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit farm mutations: %w", err)
	}
	return nil
}

func validateFarmMutations(mutations []*farmv1.FarmWriteMutation) (bool, error) {
	hasFarmRows := false
	for _, mutation := range mutations {
		if mutation == nil {
			return false, errors.New("store: invalid farm mutation")
		}
		hasRows := mutation.PlayerMask != 0 || len(mutation.Plots) != 0 || len(mutation.Items) != 0 ||
			len(mutation.Codex) != 0 || len(mutation.Outbox) != 0
		if mutation.Uid == 0 || hasRows && mutation.FarmSeq == 0 && !isLegacyZeroSequenceCrossVisitor(mutation) {
			return false, fmt.Errorf(
				"store: invalid farm mutation: uid=%d farm_seq=%d has_farm_rows=%t",
				mutation.Uid,
				mutation.FarmSeq,
				hasRows,
			)
		}
		if hasRows {
			hasFarmRows = true
		}
	}
	return hasFarmRows, nil
}

// Early cross-farm maintenance records changed only absolute visitor fields
// before that path began advancing FarmSeq. They are safe to replay only onto
// an equally unsequenced player row; all other farm-row mutations still
// require a positive sequence.
func isLegacyZeroSequenceCrossVisitor(mutation *farmv1.FarmWriteMutation) bool {
	const legacyMask = outbox.PlayerEconomy | outbox.PlayerDaily | outbox.PlayerCrossPending
	return mutation != nil &&
		mutation.FarmSeq == 0 &&
		mutation.PlayerMask == legacyMask &&
		len(mutation.Plots) == 0 &&
		len(mutation.Items) == 0 &&
		len(mutation.Codex) == 0 &&
		len(mutation.Outbox) == 0
}

func materializeMutationPlayers(ctx context.Context, tx *sql.Tx, mutations []*farmv1.FarmWriteMutation, now int64) error {
	filtered := mutations[:0]
	for _, mutation := range mutations {
		if mutation.PlayerMask != 0 {
			filtered = append(filtered, mutation)
		}
	}
	mutations = filtered
	if len(mutations) == 0 {
		return nil
	}
	aliases := []string{"uid", "player_mask", "nickname", "unlocked_plots", "level_value", "exp_value", "coin",
		"codex_bitmap", "daily_blob", "pet_blob", "cross_blob", "cross_receipt_blob", "farm_seq", "updated_at"}
	rows := make([]string, 0, len(mutations))
	args := make([]any, 0, len(mutations)*len(aliases))
	for index, mutation := range mutations {
		columns := make([]string, len(aliases))
		for column, alias := range aliases {
			if alias == "codex_bitmap" && index == 0 {
				columns[column] = "CAST(? AS BINARY(8)) AS codex_bitmap"
			} else if alias == "codex_bitmap" {
				columns[column] = "CAST(? AS BINARY(8))"
			} else if index == 0 {
				columns[column] = "? AS " + alias
			} else {
				columns[column] = "?"
			}
		}
		rows = append(rows, "SELECT "+strings.Join(columns, ", "))
		args = append(args, mutation.Uid, mutation.PlayerMask, mutation.Nickname, mutation.UnlockedPlots,
			mutation.Level, mutation.Exp, mutation.Coin, mutation.CodexBitmap, mutation.DailyJson,
			mutation.PetJson, nonNilBytes(mutation.CrossPendingJson), nonNilBytes(mutation.CrossReceiptJson), mutation.FarmSeq, now)
	}
	sets := []string{
		fmt.Sprintf("p.nickname = IF((v.player_mask & %d) != 0, v.nickname, p.nickname)", outbox.PlayerIdentity),
		fmt.Sprintf("p.unlocked_plots = IF((v.player_mask & %d) != 0, v.unlocked_plots, p.unlocked_plots)", outbox.PlayerIdentity),
		fmt.Sprintf("p.level = IF((v.player_mask & %d) != 0, v.level_value, p.level)", outbox.PlayerEconomy),
		fmt.Sprintf("p.exp = IF((v.player_mask & %d) != 0, v.exp_value, p.exp)", outbox.PlayerEconomy),
		fmt.Sprintf("p.coin = IF((v.player_mask & %d) != 0, v.coin, p.coin)", outbox.PlayerEconomy),
		fmt.Sprintf("p.codex_bitmap = IF((v.player_mask & %d) != 0, v.codex_bitmap, p.codex_bitmap)", outbox.PlayerCodexBitmap),
		fmt.Sprintf("p.daily_blob = IF((v.player_mask & %d) != 0, v.daily_blob, p.daily_blob)", outbox.PlayerDaily),
		fmt.Sprintf("p.pet_blob = IF((v.player_mask & %d) != 0, v.pet_blob, p.pet_blob)", outbox.PlayerPet),
		fmt.Sprintf("p.cross_blob = IF((v.player_mask & %d) != 0, v.cross_blob, p.cross_blob)", outbox.PlayerCrossPending),
		fmt.Sprintf("p.cross_receipt_blob = IF((v.player_mask & %d) != 0, v.cross_receipt_blob, p.cross_receipt_blob)", outbox.PlayerCrossReceipts),
		"p.farm_seq = v.farm_seq", "p.updated_at = v.updated_at",
	}
	// Targeted claim projection may materialize a UID ahead of the ordinary
	// shard consumer. The consumer can subsequently replay the same absolute
	// mutation from an already-read batch; only a strictly newer FarmSeq may
	// replace player economy after a direct reward transaction.
	query := "UPDATE player AS p JOIN (" + strings.Join(rows, " UNION ALL ") + ") AS v ON p.uid = v.uid SET " +
		strings.Join(sets, ", ") +
		" WHERE v.farm_seq > p.farm_seq OR (v.farm_seq = 0 AND p.farm_seq = 0)"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: batch update mutation players: %w", err)
	}
	return nil
}

func nonNilBytes(value []byte) []byte {
	if value == nil {
		return []byte{}
	}
	return value
}

func mutationPlayerAliases(mask uint32) []string {
	aliases := []string{"uid"}
	if mask&outbox.PlayerIdentity != 0 {
		aliases = append(aliases, "nickname", "unlocked_plots")
	}
	if mask&outbox.PlayerEconomy != 0 {
		aliases = append(aliases, "level_value", "exp_value", "coin")
	}
	if mask&outbox.PlayerCodexBitmap != 0 {
		aliases = append(aliases, "codex_bitmap")
	}
	if mask&outbox.PlayerDaily != 0 {
		aliases = append(aliases, "daily_blob")
	}
	if mask&outbox.PlayerPet != 0 {
		aliases = append(aliases, "pet_blob")
	}
	if mask&outbox.PlayerCrossPending != 0 {
		aliases = append(aliases, "cross_blob")
	}
	if mask&outbox.PlayerCrossReceipts != 0 {
		aliases = append(aliases, "cross_receipt_blob")
	}
	return append(aliases, "farm_seq", "updated_at")
}

func mutationPlayerArgs(mutation *farmv1.FarmWriteMutation, mask uint32, now int64) []any {
	args := []any{mutation.Uid}
	if mask&outbox.PlayerIdentity != 0 {
		args = append(args, mutation.Nickname, mutation.UnlockedPlots)
	}
	if mask&outbox.PlayerEconomy != 0 {
		args = append(args, mutation.Level, mutation.Exp, mutation.Coin)
	}
	if mask&outbox.PlayerCodexBitmap != 0 {
		args = append(args, mutation.CodexBitmap)
	}
	if mask&outbox.PlayerDaily != 0 {
		args = append(args, mutation.DailyJson)
	}
	if mask&outbox.PlayerPet != 0 {
		args = append(args, mutation.PetJson)
	}
	if mask&outbox.PlayerCrossPending != 0 {
		args = append(args, mutation.CrossPendingJson)
	}
	if mask&outbox.PlayerCrossReceipts != 0 {
		args = append(args, mutation.CrossReceiptJson)
	}
	return append(args, mutation.FarmSeq, now)
}

func mutationPlayerSets(mask uint32) []string {
	sets := make([]string, 0, 12)
	if mask&outbox.PlayerIdentity != 0 {
		sets = append(sets, "p.nickname = v.nickname", "p.unlocked_plots = v.unlocked_plots")
	}
	if mask&outbox.PlayerEconomy != 0 {
		sets = append(sets, "p.level = v.level_value", "p.exp = v.exp_value", "p.coin = v.coin")
	}
	if mask&outbox.PlayerCodexBitmap != 0 {
		sets = append(sets, "p.codex_bitmap = v.codex_bitmap")
	}
	if mask&outbox.PlayerDaily != 0 {
		sets = append(sets, "p.daily_blob = v.daily_blob")
	}
	if mask&outbox.PlayerPet != 0 {
		sets = append(sets, "p.pet_blob = v.pet_blob")
	}
	if mask&outbox.PlayerCrossPending != 0 {
		sets = append(sets, "p.cross_blob = v.cross_blob")
	}
	if mask&outbox.PlayerCrossReceipts != 0 {
		sets = append(sets, "p.cross_receipt_blob = v.cross_receipt_blob")
	}
	return append(sets, "p.farm_seq = v.farm_seq", "p.updated_at = v.updated_at")
}

type mutationPlot struct {
	uid   uint64
	index uint32
	plot  *farmv1.FarmWritePlot
}

func materializeMutationPlots(ctx context.Context, tx *sql.Tx, mutations []*farmv1.FarmWriteMutation) error {
	plots := make([]mutationPlot, 0)
	for _, mutation := range mutations {
		for _, plot := range mutation.Plots {
			if plot == nil || plot.Index >= uint32(gameconfig.MaxPlots) {
				return errors.New("store: invalid mutation plot")
			}
			plots = append(plots, mutationPlot{uid: mutation.Uid, index: plot.Index, plot: plot})
		}
	}
	sort.Slice(plots, func(left, right int) bool {
		return plots[left].uid < plots[right].uid || plots[left].uid == plots[right].uid && plots[left].index < plots[right].index
	})
	if len(plots) == 0 {
		return nil
	}
	values := make([]string, 0, len(plots))
	args := make([]any, 0, len(plots)*3)
	for _, entry := range plots {
		blob, err := EncodePlot(decodeMutationPlot(entry.plot))
		if err != nil {
			return fmt.Errorf("store: encode mutation plot: %w", err)
		}
		values = append(values, "(?, ?, ?)")
		args = append(args, entry.uid, entry.index, blob)
	}
	query := "INSERT INTO farm_plot (uid, plot_index, `blob`) VALUES " + strings.Join(values, ",") +
		" ON DUPLICATE KEY UPDATE `blob` = VALUES(`blob`)"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: upsert mutation plots: %w", err)
	}
	return nil
}

func decodeMutationPlot(plot *farmv1.FarmWritePlot) farm.Plot {
	return farm.Plot{
		State: uint8(plot.State), SeasonIndex: uint8(plot.SeasonIndex), SeasonTotal: uint8(plot.SeasonTotal),
		StageCount: uint8(plot.StageCount), FertMask: uint8(plot.FertMask),
		WeedNextWin: uint8(plot.WeedNextWin), PestNextWin: uint8(plot.PestNextWin),
		CropID: uint16(plot.CropId), FinalYield: uint16(plot.FinalYield), StolenCount: uint16(plot.StolenCount),
		PlantNonce: plot.PlantNonce, HarvestRound: plot.HarvestRound,
		SeasonStartAt: plot.SeasonStartAt, SeasonDuration: plot.SeasonDuration, MatureAt: plot.MatureAt,
		LastSettleAt: plot.LastSettleAt, LastWaterAt: plot.LastWaterAt, WeedSince: plot.WeedSince,
		PestSince: plot.PestSince, AccruedWeighted: plot.AccruedWeighted,
		Stealers: append([]uint64(nil), plot.Stealers...),
	}
}

type mutationItem struct {
	uid    uint64
	key    farm.ItemKey
	kind   uint8
	itemID uint16
	count  uint32
}

func materializeMutationItems(ctx context.Context, tx *sql.Tx, mutations []*farmv1.FarmWriteMutation) error {
	byKey := make(map[string]mutationItem)
	replace := make(map[uint64]map[farm.ItemKey]struct{})
	for _, mutation := range mutations {
		if mutation.ReplaceItems {
			replace[mutation.Uid] = make(map[farm.ItemKey]struct{}, len(mutation.Items))
		}
		for _, item := range mutation.Items {
			key := farm.ItemKey(item.Key)
			kind, itemID, err := ParseItemKey(key)
			if err != nil {
				return fmt.Errorf("store: parse mutation item %q: %w", key, err)
			}
			entry := mutationItem{uid: mutation.Uid, key: key, kind: kind, itemID: itemID, count: item.Count}
			byKey[fmt.Sprintf("%020d:%03d:%05d", entry.uid, kind, itemID)] = entry
			if mutation.ReplaceItems {
				replace[mutation.Uid][key] = struct{}{}
			}
		}
	}
	if len(replace) > 0 {
		uids := make([]uint64, 0, len(replace))
		for uid := range replace {
			uids = append(uids, uid)
		}
		sort.Slice(uids, func(left, right int) bool { return uids[left] < uids[right] })
		marks := make([]string, len(uids))
		args := make([]any, len(uids))
		for index, uid := range uids {
			marks[index] = "?"
			args[index] = uid
		}
		rows, err := tx.QueryContext(ctx, "SELECT uid, kind, item_id FROM item WHERE uid IN ("+
			strings.Join(marks, ",")+") ORDER BY uid, kind, item_id FOR UPDATE", args...)
		if err != nil {
			return fmt.Errorf("store: lock replacement items: %w", err)
		}
		for rows.Next() {
			var uid uint64
			var kind uint8
			var itemID uint16
			if err := rows.Scan(&uid, &kind, &itemID); err != nil {
				rows.Close()
				return err
			}
			key, err := FormatItemKey(kind, itemID)
			if err != nil {
				rows.Close()
				return err
			}
			if _, keep := replace[uid][key]; !keep {
				byKey[fmt.Sprintf("%020d:%03d:%05d", uid, kind, itemID)] = mutationItem{uid: uid, key: key, kind: kind, itemID: itemID}
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	orderedKeys := make([]string, 0, len(byKey))
	for key := range byKey {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)
	deletes, upserts := make([]mutationItem, 0), make([]mutationItem, 0)
	for _, key := range orderedKeys {
		entry := byKey[key]
		if entry.count == 0 {
			deletes = append(deletes, entry)
		} else {
			upserts = append(upserts, entry)
		}
	}
	if len(deletes) > 0 {
		values := make([]string, len(deletes))
		args := make([]any, 0, len(deletes)*3)
		for index, entry := range deletes {
			values[index] = "(?, ?, ?)"
			args = append(args, entry.uid, entry.kind, entry.itemID)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM item WHERE (uid, kind, item_id) IN ("+strings.Join(values, ",")+")", args...); err != nil {
			return fmt.Errorf("store: delete exact mutation items: %w", err)
		}
	}
	if len(upserts) > 0 {
		values := make([]string, len(upserts))
		args := make([]any, 0, len(upserts)*4)
		for index, entry := range upserts {
			values[index] = "(?, ?, ?, ?)"
			args = append(args, entry.uid, entry.kind, entry.itemID, entry.count)
		}
		query := "INSERT INTO item (uid, kind, item_id, count) VALUES " + strings.Join(values, ",") +
			" ON DUPLICATE KEY UPDATE count = VALUES(count)"
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("store: upsert mutation items: %w", err)
		}
	}
	return nil
}

func materializeMutationCodex(ctx context.Context, tx *sql.Tx, mutations []*farmv1.FarmWriteMutation, now int64) error {
	type entry struct {
		uid    uint64
		cropID uint32
		count  uint32
	}
	entries := make([]entry, 0)
	for _, mutation := range mutations {
		for _, codex := range mutation.Codex {
			if codex.CropId == 0 || codex.CropId > 0xffff || codex.HarvestCount == 0 {
				continue
			}
			entries = append(entries, entry{mutation.Uid, codex.CropId, codex.HarvestCount})
		}
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].uid < entries[right].uid || entries[left].uid == entries[right].uid && entries[left].cropID < entries[right].cropID
	})
	if len(entries) == 0 {
		return nil
	}
	values := make([]string, len(entries))
	args := make([]any, 0, len(entries)*4)
	for index, entry := range entries {
		values[index] = "(?, ?, ?, ?)"
		args = append(args, entry.uid, entry.cropID, entry.count, now)
	}
	query := "INSERT INTO player_codex (uid, crop_id, harvest_count, updated_at) VALUES " + strings.Join(values, ",") +
		" ON DUPLICATE KEY UPDATE harvest_count = GREATEST(harvest_count, VALUES(harvest_count)), updated_at = VALUES(updated_at)"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: upsert mutation codex: %w", err)
	}
	return nil
}

func mutationOutboxEvents(mutations []*farmv1.FarmWriteMutation) []outbox.Event {
	events := make([]outbox.Event, 0)
	for _, mutation := range mutations {
		for _, event := range mutation.Outbox {
			events = append(events, outbox.Event{EventID: event.EventId, ProducerUID: event.ProducerUid,
				TargetUID: event.TargetUid, Kind: outbox.Kind(event.Kind), Payload: event.Payload})
		}
	}
	return events
}
