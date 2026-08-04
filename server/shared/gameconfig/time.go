// 时间档配置（期 2）。
//
// 数值对照 docs/design/game-design-full.md 3.2 节及
// client/src/game/config.js 的 TIME_SCALES：1 缩放小时对应的真实毫秒数。
//
// 服务端默认 demo 档（1h=6s），单测/smoke 通过注入 now 或切 bench 验证，
// 禁止依赖真实长等待（见期 2 规格 4.5）。
package gameconfig

import (
	"sync/atomic"
	"time"
)

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

// ValidTimeProfile reports whether profile is a supported runtime profile.
// The browser-visible setting and every authoritative Farm process must use
// the same value; unknown values are rejected during process startup.
func ValidTimeProfile(profile string) bool {
	return HourMs(profile) > 0
}

// TimeProfileSwitch stores the process-wide authoritative time profile.
//
// Debug builds may update it while requests are in flight, so readers must not
// access a plain string shared between HTTP/WS goroutines. Farm access applies
// the active value to persisted growing crops while preserving their progress;
// new seasons, pet feeding intervals and logic-day calculations also read it.
type TimeProfileSwitch struct {
	value atomic.Value
}

// NewTimeProfileSwitch creates a runtime profile switch. Invalid input falls
// back to demo; process startup performs stricter validation before this point.
func NewTimeProfileSwitch(profile string) *TimeProfileSwitch {
	if !ValidTimeProfile(profile) {
		profile = TimeProfileDemo
	}
	profiles := &TimeProfileSwitch{}
	profiles.value.Store(profile)
	return profiles
}

// Get returns the currently active authoritative profile.
func (profiles *TimeProfileSwitch) Get() string {
	if profiles == nil {
		return TimeProfileDemo
	}
	value, ok := profiles.value.Load().(string)
	if !ok || !ValidTimeProfile(value) {
		return TimeProfileDemo
	}
	return value
}

// Set atomically switches to profile. Unknown profiles are rejected.
func (profiles *TimeProfileSwitch) Set(profile string) bool {
	if profiles == nil || !ValidTimeProfile(profile) {
		return false
	}
	profiles.value.Store(profile)
	return true
}

// LogicDayMs 返回某时间档下逻辑日的真实毫秒数，受 LogicDayMinMs 兜底。
func LogicDayMs(profile string) int64 {
	d := 24 * HourMs(profile)
	if d < LogicDayMinMs {
		return LogicDayMinMs
	}
	return d
}

// LogicDayID 返回 now 所属逻辑日的编号。
//
// 仅供跟随时间档缩放的农场 demo 玩法状态（如维护额度）判断「是否换天」。各模块
// 不应自行用常量除法：demo 档下 24 缩放小时只有 144 秒，被 LogicDayMinMs 抬到
// 5 分钟，而 fast 档是实打实的 24 分钟。每日任务与每日登录改用 LocalDayKey。
func LogicDayID(profile string, now int64) uint32 {
	if now <= 0 {
		return 0
	}
	return uint32(now / LogicDayMs(profile))
}

// LocalDayKey returns the server-local calendar day containing nowMs, encoded
// as YYYYMMDD. Daily tasks use this real-world boundary; it is deliberately
// independent from the accelerated demo LogicDayID used by farm simulation.
func LocalDayKey(nowMs int64) int64 {
	now := time.UnixMilli(nowMs).In(time.Local)
	return int64(now.Year()*10_000 + int(now.Month())*100 + now.Day())
}

// NextLocalDayResetMs returns the Unix-millisecond timestamp of the next
// server-local midnight after nowMs.
func NextLocalDayResetMs(nowMs int64) int64 {
	now := time.UnixMilli(nowMs).In(time.Local)
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.Local)
	return next.UnixMilli()
}

// LocalDayBounds returns the server-local [start, nextStart) range for a
// YYYYMMDD key. It is used when mapping legacy records written before daily
// tasks switched away from accelerated logical days.
func LocalDayBounds(dayKey int64) (startMs, nextStartMs int64, ok bool) {
	year := int(dayKey / 10_000)
	month := time.Month(dayKey / 100 % 100)
	day := int(dayKey % 100)
	if year < 1 || month < time.January || month > time.December || day < 1 || day > 31 {
		return 0, 0, false
	}
	start := time.Date(year, month, day, 0, 0, 0, 0, time.Local)
	if start.Year() != year || start.Month() != month || start.Day() != day {
		return 0, 0, false
	}
	return start.UnixMilli(), start.AddDate(0, 0, 1).UnixMilli(), true
}
