// Package gameconfig 存放期 1 手写的最小游戏常量（完整 CSV 代码生成管线后置）。
//
// 数值依据 docs/design/game-design-full.md 4.2 节；ConfigVer 依据
// docs/superpowers/specs/2026-07-26-phase1-login-farm-snapshot.md 4.2 节
// （期 1 常量固定为 1，与客户端 Handshake.client_config_ver 比对）。
package gameconfig

const (
	// ConfigVer 是本期固定的配置版本号，Handshake 时用于比对，不一致返回 ERR_CONFIG_STALE。
	ConfigVer = 1

	// InitialCoin 是新玩家的初始金币。
	InitialCoin = 1000

	// InitialUnlockedPlots 是新玩家的初始已解锁地块数。
	InitialUnlockedPlots = 6

	// MaxPlots 是农场地块总数上限。
	MaxPlots = 18

	// ExpPerLevel 是到达等级 N 所需的累计经验分母：Level = Exp / ExpPerLevel。
	// 与客户端 EXP_PER_LEVEL、策划 4.3 节一致（累计门槛 = N × 200）。
	ExpPerLevel = 200
)

// FriendLimit 是单个玩家可建立的好友关系上限。用变量便于存储层测试注入较小阈值。
var FriendLimit = 200
