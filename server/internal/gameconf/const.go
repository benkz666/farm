// Package gameconf 存放期 1 手写的最小游戏常量（完整 CSV 代码生成管线后置）。
//
// 数值依据 docs/design/game-design-full.md 4.2 节；ConfigVer 依据
// docs/superpowers/specs/2026-07-26-phase1-login-farm-snapshot.md 4.2 节
// （期 1 常量固定为 1，与客户端 Handshake.client_config_ver 比对）。
package gameconf

const (
	// ConfigVer 是本期固定的配置版本号，Handshake 时用于比对，不一致返回 ERR_CONFIG_STALE。
	ConfigVer = 1

	// InitialCoin 是新玩家的初始金币。
	InitialCoin = 1000

	// InitialUnlockedPlots 是新玩家的初始已解锁地块数。
	InitialUnlockedPlots = 6

	// MaxPlots 是农场地块总数上限。
	MaxPlots = 18
)
