package farm

import (
	"sort"

	"farm/server/internal/gameconf"
)

// CodexProgress is the authoritative per-crop collection plaque state.
type CodexProgress struct {
	CropID       uint16 `json:"crop_id"`
	HarvestCount uint32 `json:"harvest_count"`
	Tier         string `json:"tier"`
	NextTarget   uint32 `json:"next_target"`
}

// CodexRewardNotice tells the harvesting client which plaque reward mails were
// newly created. Coins remain in the mail attachment until explicitly claimed.
type CodexRewardNotice struct {
	CropID     uint16 `json:"crop_id"`
	Tier       string `json:"tier"`
	Target     uint32 `json:"target"`
	RewardCoin int64  `json:"reward_coin"`
}

// RecordCodexHarvest counts one successful owner harvest action. Yield is
// intentionally irrelevant: one harvested plot/season always adds exactly one.
func (a *Aggregate) RecordCodexHarvest(cropID uint16) CodexProgress {
	if a.CodexHarvests == nil {
		a.CodexHarvests = make(map[uint16]uint32)
	}
	a.CodexHarvests[cropID]++
	return CodexProgressOf(cropID, a.CodexHarvests[cropID])
}

// CodexProgressOf derives the material plaque stage from one persisted count.
func CodexProgressOf(cropID uint16, harvestCount uint32) CodexProgress {
	tier, nextTarget := gameconf.CodexTierAt(harvestCount)
	return CodexProgress{
		CropID:       cropID,
		HarvestCount: harvestCount,
		Tier:         tier,
		NextTarget:   nextTarget,
	}
}

// CodexSnapshot returns unlocked crop plaques in stable numeric crop order.
func (a *Aggregate) CodexSnapshot() []CodexProgress {
	if a == nil || len(a.CodexHarvests) == 0 {
		return []CodexProgress{}
	}
	cropIDs := make([]int, 0, len(a.CodexHarvests))
	for cropID, count := range a.CodexHarvests {
		if cropID != 0 && count != 0 {
			cropIDs = append(cropIDs, int(cropID))
		}
	}
	sort.Ints(cropIDs)
	result := make([]CodexProgress, 0, len(cropIDs))
	for _, cropID := range cropIDs {
		id := uint16(cropID)
		result = append(result, CodexProgressOf(id, a.CodexHarvests[id]))
	}
	return result
}
