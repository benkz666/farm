// Package gameconf 提供玩法静态配置。
//
// 作物数值由 config/crops.csv 经 tools/gen-config 生成到 gen_crops.go。
// 禁止手改 gen_*.go；改数值请编辑 CSV 后执行 make gen。
package gameconf

// CropConf 描述一种作物的静态数值。
type CropConf struct {
	ID           uint16
	Slug         string // 客户端字符串 id（如 bailuobo）
	Name         string // 中文名
	UnlockLevel  uint8  // 解锁所需玩家等级（隐藏作物等同掉落门槛）
	SeedPrice    uint32 // 种子单价；隐藏作物为 0
	FruitPrice   uint32 // 果实单价
	Yield        uint16 // 名义产量（健康度 100% 时的产出）
	Seasons      uint8  // 总季数（单季作物为 1）
	CycleMinutes uint32 // 全周期缩放分钟（权威时长；多季按 SeasonMinutes 拆分）
	HarvestExp   uint32 // 单次收获经验
	Hidden       bool   // 商店不出售，仅锄地/清理掉落
	DropLevel    uint8  // 隐藏种子最低掉落等级
}

// CropByID 查表，找不到返回 false。id==0 视为「无作物」返回 false。
func CropByID(id uint16) (CropConf, bool) {
	if id == 0 || int(id) > len(cropTable) {
		return CropConf{}, false
	}
	return cropTable[id-1], true
}

// stealCompensationMultiple 是被看家狗抓住时按果实单价赔付的倍数（策划 12.4）。
const stealCompensationMultiple = 10

// StealCompensation 返回偷取某作物被拦截时访客应赔付的金币。
//
// 发起偷菜、访客侧冻结、主人侧校验三处必须调同一个函数：任一处的倍数漂移，轻则
// 让偷菜永远校验不通过，重则让冻结额与赔付额不等而产生资损。
func StealCompensation(crop CropConf) int64 {
	return int64(crop.FruitPrice) * stealCompensationMultiple
}

// SeasonMinutes 按策划 6.3 的多季拆分规则返回当前季的缩放分钟数。
//
//	后续每季 = CycleMinutes / (Seasons+1)
//	首季     = 2 × 后续每季
//
// 权威单位为整数分钟，禁止用小时整数除法截断。
func SeasonMinutes(crop CropConf, seasonIndex uint8) uint32 {
	if crop.Seasons <= 1 {
		return crop.CycleMinutes
	}
	later := crop.CycleMinutes / uint32(crop.Seasons+1)
	if seasonIndex == 0 {
		return later * 2
	}
	return later
}

// SeasonDurationMs 将本季分钟数换算为指定时间档的真实毫秒。
// 要求 HourMs(profile) 能被 60 整除，否则返回 0（装配期应拒绝非法档）。
func SeasonDurationMs(crop CropConf, seasonIndex uint8, profile string) int64 {
	hour := HourMs(profile)
	if hour == 0 || hour%60 != 0 {
		return 0
	}
	return int64(SeasonMinutes(crop, seasonIndex)) * hour / 60
}
