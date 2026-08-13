// Package gateway implements the HTTP and binary WebSocket boundary.
package gateway

import (
	"farm/server/shared/clientwire"
)

const (
	CommandHandshake uint32 = 100
	CommandPing      uint32 = 102
	CommandEnterFarm uint32 = 200
	CommandLeaveFarm uint32 = 202
	CommandSyncFarm  uint32 = 204

	CommandTill                uint32 = 206
	CommandClear               uint32 = 208
	CommandPlant               uint32 = 210
	CommandWater               uint32 = 212
	CommandRemoveWeed          uint32 = 214
	CommandRemovePest          uint32 = 216
	CommandFertilize           uint32 = 218
	CommandHarvest             uint32 = 220
	CommandSteal               uint32 = 222
	CommandBuy                 uint32 = 302
	CommandSell                uint32 = 304
	CommandFriendList          uint32 = 400
	CommandGenShareLink        uint32 = 402
	CommandAcceptInvite        uint32 = 404
	CommandRemoveFriend        uint32 = 406
	CommandAddFriendByUID      uint32 = 408
	CommandSearchUser          uint32 = 410
	CommandRequestFriend       uint32 = 412
	CommandListFriendRequests  uint32 = 414
	CommandAcceptFriendRequest uint32 = 416
	CommandRejectFriendRequest uint32 = 418
	CommandPetStatus           uint32 = 500
	CommandPetActivate         uint32 = 502
	CommandPetFeed             uint32 = 504
	CommandTaskList            uint32 = 600
	CommandTaskClaim           uint32 = 602
	CommandMailList            uint32 = 604
	CommandMailRead            uint32 = 606
	CommandMailClaim           uint32 = 608
	CommandMailDelete          uint32 = 610
	CommandCodexList           uint32 = 612
	CommandClaimDailyLogin     uint32 = 614
	CommandSetTimeProfile      uint32 = 616

	CommandFarmDelta   = clientwire.CommandFarmDelta
	CommandPlayerDelta = clientwire.CommandPlayerDelta
	CommandMailNotify  = clientwire.CommandMailNotify
	CommandSessionKick = clientwire.CommandSessionKick
	CommandTaskNotify  = clientwire.CommandTaskNotify

	BinarySubprotocol = clientwire.BinarySubprotocol
)
