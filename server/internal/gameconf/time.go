// 时间档配置（期 2）。
//
// 数值对照 docs/design/game-design-full.md 3.2 节及
// client/src/game/config.js 的 TIME_SCALES：1 缩放小时对应的真实毫秒数。
//
// 服务端默认 demo 档（1h=6s），单测/smoke 通过注入 now 或切 bench 验证，
// 禁止依赖真实长等待（见期 2 规格 4.5）。
package gameconf

// 时间档名称常量，避免魔法字符串散落。
const (
	TimeProfileDemo      = "demo"
	TimeProfileFast      = "fast"
	TimeProfileAuthentic = "authentic"
)

// hourMsTable 与客户端 TIME_SCALES.hourMs 严格一致。
var hourMsTable = map[string]int64{
	TimeProfileDemo:      6_000,
	TimeProfileFast:      60_000,
	TimeProfileAuthentic: 3_600_000,
}

// LogicDayMinMs 是逻辑日真实时长下限（5 分钟），见策划 3.4 节。
const LogicDayMinMs int64 = 5 * 60 * 1000

// HourMs 返回某时间档下 1 缩放小时对应的真实毫秒数。
// profile 不存在时返回 0，调用方应在装配阶段校验。
func HourMs(profile string) int64 {
	return hourMsTable[profile]
}

// LogicDayMs 返回某时间档下逻辑日的真实毫秒数，受 LogicDayMinMs 兜底。
func LogicDayMs(profile string) int64 {
	d := 24 * HourMs(profile)
	if d < LogicDayMinMs {
		return LogicDayMinMs
	}
	return d
}
