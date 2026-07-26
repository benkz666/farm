// 作物配置子集（期 2）。
//
// 数值严格对照 docs/design/game-design-full.md 18 章及
// client/src/game/config.js 的 CROPS 表，避免服务端权威与客户端分叉。
// 完整 CSV → Go/JS 代码生成管线后置（见期 2 技术债登记）。
//
// ID 与客户端字符串 id 的对应：
//
//	1  bailuobo  白萝卜
//	2  huluobo   胡萝卜
//	3  dabaicai  大白菜
//
// 0 保留为「无作物」，CropByID(0) 返回 false。
package gameconf

// CropConf 描述一种作物的静态数值。
type CropConf struct {
	ID          uint16
	UnlockLevel uint8  // 解锁所需玩家等级
	SeedPrice   uint32 // 种子单价
	FruitPrice  uint32 // 果实单价
	Yield       uint16 // 名义产量（健康度 100% 时的产出）
	Seasons     uint8  // 总季数（单季作物为 1）
	CycleHours  uint16 // 单季生长时长（缩放小时）
	HarvestExp  uint32 // 单次收获经验
}

// cropTable 是期 2 手写的最小作物子集。
//
// 顺序即 ID 顺序；新增作物时按客户端 CROPS 表追加，禁止重排已有 ID。
var cropTable = []CropConf{
	{ID: 1, UnlockLevel: 0, SeedPrice: 125, FruitPrice: 17, Yield: 16, Seasons: 1, CycleHours: 10, HarvestExp: 15}, // 白萝卜
	{ID: 2, UnlockLevel: 0, SeedPrice: 163, FruitPrice: 21, Yield: 17, Seasons: 1, CycleHours: 13, HarvestExp: 18}, // 胡萝卜
	{ID: 3, UnlockLevel: 1, SeedPrice: 168, FruitPrice: 22, Yield: 17, Seasons: 1, CycleHours: 14, HarvestExp: 19}, // 大白菜
}

// CropByID 查表，找不到返回 false。id==0 视为「无作物」返回 false。
func CropByID(id uint16) (CropConf, bool) {
	if id == 0 || int(id) > len(cropTable) {
		return CropConf{}, false
	}
	return cropTable[id-1], true
}
