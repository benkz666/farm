package gameconfig

// FertilizerConf 描述一种化肥。ID 用于 Fertilize arg，ShopItemID 用于 Buy item_id。
// 两者分离，避免化肥 ID 与作物 ID 在统一商店命名空间碰撞。
type FertilizerConf struct {
	ID          uint16
	ShopItemID  uint16
	Price       uint32
	ReduceHours float64
}

var fertilizerTable = []FertilizerConf{
	{ID: 1, ShopItemID: 1_001, Price: 50, ReduceHours: 1.0},  // 普通化肥
	{ID: 2, ShopItemID: 1_002, Price: 200, ReduceHours: 2.5}, // 高速化肥
	{ID: 3, ShopItemID: 1_003, Price: 500, ReduceHours: 5.5}, // 急速化肥
}

// FertilizerByID 按协议中的 fertilizer_id 查找化肥配置。
func FertilizerByID(id uint16) (FertilizerConf, bool) {
	if id == 0 || int(id) > len(fertilizerTable) {
		return FertilizerConf{}, false
	}
	return fertilizerTable[id-1], true
}

// FertilizerByShopItemID 按商店 item_id 查找化肥配置。
func FertilizerByShopItemID(itemID uint16) (FertilizerConf, bool) {
	for _, fertilizer := range fertilizerTable {
		if fertilizer.ShopItemID == itemID {
			return fertilizer, true
		}
	}
	return FertilizerConf{}, false
}
