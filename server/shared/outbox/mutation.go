package outbox

import (
	"encoding/json"
	"errors"
	"sort"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
)

// Player projection masks keep the incremental journal independent from SQL
// statement shape. Masks are unioned while coalescing records for one UID.
const (
	PlayerIdentity uint32 = 1 << iota
	PlayerEconomy
	PlayerCodexBitmap
	PlayerDaily
	PlayerPet
	PlayerCrossPending
	PlayerCrossReceipts
)

const PlayerAll = PlayerIdentity | PlayerEconomy | PlayerCodexBitmap |
	PlayerDaily | PlayerPet | PlayerCrossPending | PlayerCrossReceipts

// NewFarmWriteMutation detaches only the rows touched by the pending Actor
// generations. Full mode remains available for rare administrative paths, but
// ordinary gameplay never clones or JSON-encodes the complete Aggregate.
func NewFarmWriteMutation(
	agg *farm.Aggregate,
	plan PersistPlan,
	plotIndexes []uint8,
	itemCounts map[farm.ItemKey]uint32,
	codexIDs []uint16,
	events []Event,
	tasks []TaskAdvance,
	rewards []CodexReward,
	claims []TaskClaim,
	mailMutations []MailMutation,
) (*farmv1.FarmWriteMutation, error) {
	if agg == nil || agg.UID == 0 {
		return nil, errors.New("outbox: invalid farm mutation aggregate")
	}
	mutation := &farmv1.FarmWriteMutation{
		Uid: agg.UID, FarmSeq: agg.FarmSeq,
		Level: uint32(agg.Level), Exp: agg.Exp, Coin: agg.Coin,
	}
	modes := plan.Modes
	if modes == 0 {
		modes = PersistModeMask(plan.Mode)
	}
	if modes&PersistModeMask(PersistFull) != 0 {
		mutation.PlayerMask = PlayerAll
		mutation.Nickname = agg.Nickname
		mutation.UnlockedPlots = uint32(agg.UnlockedPlots)
		mutation.ReplaceItems = true
		mutation.ReplaceCodex = true
		plotIndexes = make([]uint8, len(agg.Plots))
		for index := range agg.Plots {
			plotIndexes[index] = uint8(index)
		}
		itemCounts = agg.Items
		codexIDs = codexIDs[:0]
		for cropID := range agg.CodexHarvests {
			codexIDs = append(codexIDs, cropID)
		}
	} else {
		if modes&PersistModeMask(PersistEconomy) != 0 {
			mutation.PlayerMask |= PlayerEconomy | PlayerPet
		}
		if modes&PersistModeMask(PersistPlot) != 0 {
			mutation.PlayerMask |= PlayerEconomy
			if plan.IncludeCodex {
				mutation.PlayerMask |= PlayerCodexBitmap
			}
		}
		if modes&PersistModeMask(PersistCrossVisitor) != 0 {
			mutation.PlayerMask |= PlayerEconomy | PlayerDaily | PlayerCrossPending
		}
		if modes&PersistModeMask(PersistCrossOwner) != 0 {
			mutation.PlayerMask |= PlayerEconomy | PlayerPet | PlayerCrossReceipts
		}
		if mutation.PlayerMask == 0 && modes&PersistModeMask(PersistSideEffects) == 0 {
			return nil, errors.New("outbox: unsupported farm mutation plan")
		}
	}

	var err error
	if mutation.PlayerMask&PlayerCodexBitmap != 0 {
		mutation.CodexBitmap = codexBitmap(agg.CodexHarvests)
	}
	if mutation.PlayerMask&PlayerDaily != 0 {
		mutation.DailyJson, err = json.Marshal(agg.Daily)
		if err != nil {
			return nil, err
		}
	}
	if mutation.PlayerMask&PlayerPet != 0 {
		mutation.PetJson, err = json.Marshal(agg.Pet)
		if err != nil {
			return nil, err
		}
	}
	if mutation.PlayerMask&PlayerCrossPending != 0 && len(agg.CrossPending) > 0 {
		mutation.CrossPendingJson, err = json.Marshal(agg.CrossPending)
		if err != nil {
			return nil, err
		}
	}
	if mutation.PlayerMask&PlayerCrossPending != 0 && mutation.CrossPendingJson == nil {
		mutation.CrossPendingJson = []byte{}
	}
	if mutation.PlayerMask&PlayerCrossReceipts != 0 && len(agg.CrossReceipts) > 0 {
		mutation.CrossReceiptJson, err = json.Marshal(agg.CrossReceipts)
		if err != nil {
			return nil, err
		}
	}
	if mutation.PlayerMask&PlayerCrossReceipts != 0 && mutation.CrossReceiptJson == nil {
		mutation.CrossReceiptJson = []byte{}
	}

	if len(plotIndexes) > 1 {
		sort.Slice(plotIndexes, func(left, right int) bool { return plotIndexes[left] < plotIndexes[right] })
	}
	mutation.Plots = make([]*farmv1.FarmWritePlot, 0, len(plotIndexes))
	for _, index := range plotIndexes {
		if int(index) >= len(agg.Plots) {
			return nil, errors.New("outbox: invalid farm mutation plot")
		}
		mutation.Plots = append(mutation.Plots, encodeWritePlot(index, agg.Plots[index]))
	}

	itemKeys := make([]farm.ItemKey, 0, len(itemCounts))
	for key := range itemCounts {
		itemKeys = append(itemKeys, key)
	}
	if len(itemKeys) > 1 {
		sort.Slice(itemKeys, func(left, right int) bool { return itemKeys[left] < itemKeys[right] })
	}
	mutation.Items = make([]*farmv1.FarmWriteItem, 0, len(itemKeys))
	for _, key := range itemKeys {
		mutation.Items = append(mutation.Items, &farmv1.FarmWriteItem{Key: string(key), Count: itemCounts[key]})
	}

	if len(codexIDs) > 1 {
		sort.Slice(codexIDs, func(left, right int) bool { return codexIDs[left] < codexIDs[right] })
	}
	mutation.Codex = make([]*farmv1.FarmWriteCodex, 0, len(codexIDs))
	for _, cropID := range codexIDs {
		mutation.Codex = append(mutation.Codex, &farmv1.FarmWriteCodex{
			CropId: uint32(cropID), HarvestCount: agg.CodexHarvests[cropID],
		})
	}
	mutation.Outbox = make([]*farmv1.FarmWriteOutbox, 0, len(events))
	for _, event := range events {
		mutation.Outbox = append(mutation.Outbox, &farmv1.FarmWriteOutbox{
			EventId: event.EventID, ProducerUid: event.ProducerUID, TargetUid: event.TargetUID,
			Kind: string(event.Kind), Payload: append([]byte(nil), event.Payload...),
		})
	}
	mutation.TaskAdvances = make([]*farmv1.FarmWriteTaskAdvance, 0, len(tasks))
	for _, task := range tasks {
		mutation.TaskAdvances = append(mutation.TaskAdvances, &farmv1.FarmWriteTaskAdvance{
			DayKey: task.DayKey, TaskId: task.TaskID, Amount: task.Amount,
		})
	}
	mutation.CodexRewards = make([]*farmv1.FarmWriteCodexReward, 0, len(rewards))
	for _, reward := range rewards {
		mutation.CodexRewards = append(mutation.CodexRewards, &farmv1.FarmWriteCodexReward{
			CropId: uint32(reward.Progress.CropID), HarvestCount: reward.Progress.HarvestCount,
		})
	}
	mutation.TaskClaims = make([]*farmv1.FarmWriteTaskClaim, 0, len(claims))
	for _, claim := range claims {
		mutation.TaskClaims = append(mutation.TaskClaims, &farmv1.FarmWriteTaskClaim{
			DayKey: claim.DayKey, TaskId: claim.TaskID, ClaimedAt: claim.ClaimedAt,
		})
	}
	mutation.MailMutations = make([]*farmv1.FarmWriteMailMutation, 0, len(mailMutations))
	for _, item := range mailMutations {
		kind := farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_UNSPECIFIED
		switch item.Kind {
		case MailRead:
			kind = farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_READ
		case MailClaim:
			kind = farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_CLAIM
		case MailDelete:
			kind = farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_DELETE
		}
		mutation.MailMutations = append(mutation.MailMutations, &farmv1.FarmWriteMailMutation{
			MailId: item.MailID, Kind: kind, OccurredAt: item.OccurredAt,
		})
	}
	return mutation, nil
}

func codexBitmap(counts map[uint16]uint32) []byte {
	bitmap := make([]byte, 8)
	for cropID, count := range counts {
		if cropID == 0 || cropID > 64 || count == 0 {
			continue
		}
		bit := cropID - 1
		bitmap[bit/8] |= 1 << (bit % 8)
	}
	return bitmap
}

func encodeWritePlot(index uint8, plot farm.Plot) *farmv1.FarmWritePlot {
	return &farmv1.FarmWritePlot{
		Index: uint32(index), State: uint32(plot.State), SeasonIndex: uint32(plot.SeasonIndex),
		SeasonTotal: uint32(plot.SeasonTotal), StageCount: uint32(plot.StageCount),
		FertMask: uint32(plot.FertMask), WeedNextWin: uint32(plot.WeedNextWin),
		PestNextWin: uint32(plot.PestNextWin), CropId: uint32(plot.CropID),
		FinalYield: uint32(plot.FinalYield), StolenCount: uint32(plot.StolenCount),
		PlantNonce: plot.PlantNonce, HarvestRound: plot.HarvestRound,
		SeasonStartAt: plot.SeasonStartAt, SeasonDuration: plot.SeasonDuration,
		MatureAt: plot.MatureAt, LastSettleAt: plot.LastSettleAt,
		LastWaterAt: plot.LastWaterAt, WeedSince: plot.WeedSince, PestSince: plot.PestSince,
		AccruedWeighted: plot.AccruedWeighted, Stealers: append([]uint64(nil), plot.Stealers...),
	}
}
