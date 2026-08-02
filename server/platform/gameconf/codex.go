package gameconf

// CodexTierConf describes one per-crop harvest milestone. HarvestCount counts
// successful harvest actions, never the number of fruit yielded.
type CodexTierConf struct {
	Tier         string
	HarvestCount uint32
	RewardCoin   int64
}

var CodexTiers = [...]CodexTierConf{
	{Tier: "bronze", HarvestCount: 10, RewardCoin: 500},
	{Tier: "silver", HarvestCount: 20, RewardCoin: 1000},
	{Tier: "gold", HarvestCount: 50, RewardCoin: 2000},
}

// CodexTierAt returns the material tier and next target for one crop.
func CodexTierAt(harvestCount uint32) (tier string, nextTarget uint32) {
	tier = "wood"
	for _, milestone := range CodexTiers {
		if harvestCount < milestone.HarvestCount {
			return tier, milestone.HarvestCount
		}
		tier = milestone.Tier
	}
	return tier, 0
}
