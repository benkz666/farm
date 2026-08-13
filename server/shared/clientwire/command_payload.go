package clientwire

import (
	"bytes"
	"encoding/json"
	"fmt"

	"farm/server/domain/farm"
	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/clientjson"
	"farm/server/shared/store"
)

const (
	commandHandshake uint32 = 100
	commandPing      uint32 = 102
	commandLeaveFarm uint32 = 202
	commandTill      uint32 = 206
	commandClear     uint32 = 208
	commandPlant     uint32 = 210
	commandWater     uint32 = 212
	commandWeed      uint32 = 214
	commandPest      uint32 = 216
	commandFertilize uint32 = 218
	commandHarvest   uint32 = 220
	commandSteal     uint32 = 222
	commandBuy       uint32 = 302
	commandSell      uint32 = 304

	commandFriendList          uint32 = 400
	commandGenShareLink        uint32 = 402
	commandAcceptInvite        uint32 = 404
	commandRemoveFriend        uint32 = 406
	commandAddFriendByUID      uint32 = 408
	commandSearchUser          uint32 = 410
	commandRequestFriend       uint32 = 412
	commandListFriendRequests  uint32 = 414
	commandAcceptFriendRequest uint32 = 416
	commandRejectFriendRequest uint32 = 418

	commandPetStatus   uint32 = 500
	commandPetActivate uint32 = 502
	commandPetFeed     uint32 = 504

	commandTaskList        uint32 = 600
	commandTaskClaim       uint32 = 602
	commandMailList        uint32 = 604
	commandMailRead        uint32 = 606
	commandMailClaim       uint32 = 608
	commandMailDelete      uint32 = 610
	commandCodexList       uint32 = 612
	commandDailyLoginClaim uint32 = 614
	commandSetTimeProfile  uint32 = 616
)

type handshakeRequestJSON struct {
	Token           string            `json:"token"`
	ResumeFarmUID   clientjson.UID    `json:"resume_farm_uid"`
	ResumeFarmSeq   clientjson.Uint64 `json:"resume_farm_seq"`
	ClientConfigVer uint32            `json:"client_config_ver"`
}

type pingRequestJSON struct {
	ClientTime int64 `json:"client_time"`
}

type plotActionRequestJSON struct {
	OwnerUID  clientjson.UID `json:"owner_uid"`
	PlotIndex uint32         `json:"plot_index"`
	Arg       uint32         `json:"arg"`
}

type stealRequestJSON struct {
	OwnerUID  clientjson.UID `json:"owner_uid"`
	PlotIndex uint32         `json:"plot_index"`
	CropID    uint32         `json:"crop_id"`
}

type shopRequestJSON struct {
	ItemID   uint32 `json:"item_id"`
	Quantity uint32 `json:"quantity"`
}

type peerRequestJSON struct {
	PeerUID clientjson.UID `json:"peer_uid"`
}

type fromRequestJSON struct {
	FromUID clientjson.UID `json:"from_uid"`
}

type usernameRequestJSON struct {
	Username string `json:"username"`
}

type inviteRequestJSON struct {
	Token string `json:"token"`
}

type petActivateRequestJSON struct {
	DogType uint32 `json:"dog_type"`
}

type petFeedRequestJSON struct {
	Grams uint32 `json:"grams"`
}

type taskRequestJSON struct {
	TaskID uint32 `json:"task_id"`
}

type mailRequestJSON struct {
	MailID clientjson.Uint64 `json:"mail_id"`
	All    bool              `json:"all"`
}

type timeProfileRequestJSON struct {
	TimeProfile string `json:"time_profile"`
}

const (
	requestFieldAuthToken uint32 = 1 << iota
	requestFieldResumeFarmUID
	requestFieldResumeFarmSeq
	requestFieldClientConfigVer
	requestFieldClientTime
	requestFieldOwnerUID
	requestFieldPlotIndex
	requestFieldArg
	requestFieldCropID
	requestFieldItemID
	requestFieldQuantity
	requestFieldPeerUID
	requestFieldUsername
	requestFieldInviteToken
	requestFieldFromUID
	requestFieldDogType
	requestFieldGrams
	requestFieldTaskID
	requestFieldMailID
	requestFieldAll
	requestFieldTimeProfile
	requestFieldFromSeq
)

// ValidateCommandRequest enforces the cmd-specific subset of the shared
// protobuf request. Without this check a client could smuggle fields belonging
// to another command into the generic CommandRequest message.
func ValidateCommandRequest(cmd uint32, request *publicv3.CommandRequest) error {
	if request == nil || len(request.ProtoReflect().GetUnknown()) != 0 {
		return fmt.Errorf("wireenv: invalid command request")
	}
	var allowed uint32
	switch {
	case cmd == commandHandshake:
		allowed = requestFieldAuthToken | requestFieldResumeFarmUID | requestFieldResumeFarmSeq | requestFieldClientConfigVer
	case cmd == commandPing:
		allowed = requestFieldClientTime
	case cmd == commandEnterFarm:
		allowed = 0
	case cmd == commandSyncFarm:
		allowed = requestFieldFromSeq
	case isEmptyRequestCommand(cmd):
		allowed = 0
	case isPlotCommand(cmd):
		allowed = requestFieldOwnerUID | requestFieldPlotIndex | requestFieldArg
	case cmd == commandSteal:
		allowed = requestFieldOwnerUID | requestFieldPlotIndex | requestFieldCropID
	case cmd == commandBuy || cmd == commandSell:
		allowed = requestFieldItemID | requestFieldQuantity
	case cmd == commandAcceptInvite:
		allowed = requestFieldInviteToken
	case cmd == commandRemoveFriend || cmd == commandAddFriendByUID || cmd == commandRequestFriend:
		allowed = requestFieldPeerUID
	case cmd == commandSearchUser:
		allowed = requestFieldUsername
	case cmd == commandAcceptFriendRequest || cmd == commandRejectFriendRequest:
		allowed = requestFieldFromUID
	case cmd == commandPetActivate:
		allowed = requestFieldDogType
	case cmd == commandPetFeed:
		allowed = requestFieldGrams
	case cmd == commandTaskClaim:
		allowed = requestFieldTaskID
	case cmd == commandMailRead || cmd == commandMailDelete:
		allowed = requestFieldMailID | requestFieldAll
	case cmd == commandMailClaim:
		allowed = requestFieldMailID
	case cmd == commandSetTimeProfile:
		allowed = requestFieldTimeProfile
	default:
		return fmt.Errorf("wireenv: unsupported command request %d", cmd)
	}
	if populatedCommandRequestFields(request)&^allowed != 0 {
		return fmt.Errorf("wireenv: command %d contains fields from another command", cmd)
	}
	return nil
}

func populatedCommandRequestFields(request *publicv3.CommandRequest) uint32 {
	var fields uint32
	if request.AuthToken != "" {
		fields |= requestFieldAuthToken
	}
	if request.ResumeFarmUid != 0 {
		fields |= requestFieldResumeFarmUID
	}
	if request.ResumeFarmSeq != 0 {
		fields |= requestFieldResumeFarmSeq
	}
	if request.ClientConfigVer != 0 {
		fields |= requestFieldClientConfigVer
	}
	if request.ClientTime != 0 {
		fields |= requestFieldClientTime
	}
	if request.OwnerUid != 0 {
		fields |= requestFieldOwnerUID
	}
	if request.PlotIndex != 0 {
		fields |= requestFieldPlotIndex
	}
	if request.Arg != 0 {
		fields |= requestFieldArg
	}
	if request.CropId != 0 {
		fields |= requestFieldCropID
	}
	if request.ItemId != 0 {
		fields |= requestFieldItemID
	}
	if request.Quantity != 0 {
		fields |= requestFieldQuantity
	}
	if request.PeerUid != 0 {
		fields |= requestFieldPeerUID
	}
	if request.Username != "" {
		fields |= requestFieldUsername
	}
	if request.InviteToken != "" {
		fields |= requestFieldInviteToken
	}
	if request.FromUid != 0 {
		fields |= requestFieldFromUID
	}
	if request.DogType != 0 {
		fields |= requestFieldDogType
	}
	if request.Grams != 0 {
		fields |= requestFieldGrams
	}
	if request.TaskId != 0 {
		fields |= requestFieldTaskID
	}
	if request.MailId != 0 {
		fields |= requestFieldMailID
	}
	if request.All {
		fields |= requestFieldAll
	}
	if request.TimeProfile != "" {
		fields |= requestFieldTimeProfile
	}
	if request.FromSeq != 0 {
		fields |= requestFieldFromSeq
	}
	return fields
}

func isEmptyRequestCommand(cmd uint32) bool {
	switch cmd {
	case commandLeaveFarm, commandFriendList, commandGenShareLink,
		commandListFriendRequests, commandPetStatus, commandTaskList,
		commandMailList, commandCodexList, commandDailyLoginClaim:
		return true
	default:
		return false
	}
}

func isPlotCommand(cmd uint32) bool {
	return cmd >= commandTill && cmd <= commandHarvest && cmd%2 == 0
}

func isHotClientCommand(cmd uint32) bool {
	return isPlotCommand(cmd) || cmd == commandSteal || cmd == commandBuy || cmd == commandSell
}

// CommandRequestFromJSON converts the in-process compatibility representation
// used by tests and command-line clients into the public typed request.
func CommandRequestFromJSON(cmd uint32, payload json.RawMessage) (*publicv3.CommandRequest, error) {
	request := &publicv3.CommandRequest{}
	switch {
	case cmd == commandHandshake:
		var value handshakeRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.AuthToken = value.Token
		request.ResumeFarmUid = uint64(value.ResumeFarmUID)
		request.ResumeFarmSeq = uint64(value.ResumeFarmSeq)
		request.ClientConfigVer = value.ClientConfigVer
	case cmd == commandPing:
		var value pingRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.ClientTime = value.ClientTime
	case isEmptyRequestCommand(cmd):
		if err := DecodeStrictJSON(payload, &struct{}{}); err != nil {
			return nil, err
		}
	case isPlotCommand(cmd):
		var value plotActionRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.OwnerUid, request.PlotIndex, request.Arg = uint64(value.OwnerUID), value.PlotIndex, value.Arg
	case cmd == commandSteal:
		var value stealRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.OwnerUid, request.PlotIndex, request.CropId = uint64(value.OwnerUID), value.PlotIndex, value.CropID
	case cmd == commandBuy || cmd == commandSell:
		var value shopRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.ItemId, request.Quantity = value.ItemID, value.Quantity
	case cmd == commandAcceptInvite:
		var value inviteRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.InviteToken = value.Token
	case cmd == commandRemoveFriend || cmd == commandAddFriendByUID || cmd == commandRequestFriend:
		var value peerRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.PeerUid = uint64(value.PeerUID)
	case cmd == commandSearchUser:
		var value usernameRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.Username = value.Username
	case cmd == commandAcceptFriendRequest || cmd == commandRejectFriendRequest:
		var value fromRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.FromUid = uint64(value.FromUID)
	case cmd == commandPetActivate:
		var value petActivateRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.DogType = value.DogType
	case cmd == commandPetFeed:
		var value petFeedRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.Grams = value.Grams
	case cmd == commandTaskClaim:
		var value taskRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.TaskId = value.TaskID
	case cmd == commandMailRead || cmd == commandMailDelete || cmd == commandMailClaim:
		var value mailRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.MailId, request.All = uint64(value.MailID), value.All
	case cmd == commandSetTimeProfile:
		var value timeProfileRequestJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		request.TimeProfile = value.TimeProfile
	default:
		return nil, fmt.Errorf("wireenv: unsupported command request %d", cmd)
	}
	return request, nil
}

func CommandRequestToJSON(cmd uint32, request *publicv3.CommandRequest) (json.RawMessage, error) {
	if request == nil {
		return nil, fmt.Errorf("wireenv: nil command request")
	}
	var value any
	switch {
	case cmd == commandHandshake:
		value = handshakeRequestJSON{Token: request.AuthToken, ResumeFarmUID: clientjson.UID(request.ResumeFarmUid), ResumeFarmSeq: clientjson.Uint64(request.ResumeFarmSeq), ClientConfigVer: request.ClientConfigVer}
	case cmd == commandPing:
		value = pingRequestJSON{ClientTime: request.ClientTime}
	case isEmptyRequestCommand(cmd):
		value = struct{}{}
	case isPlotCommand(cmd):
		value = plotActionRequestJSON{OwnerUID: clientjson.UID(request.OwnerUid), PlotIndex: request.PlotIndex, Arg: request.Arg}
	case cmd == commandSteal:
		value = stealRequestJSON{OwnerUID: clientjson.UID(request.OwnerUid), PlotIndex: request.PlotIndex, CropID: request.CropId}
	case cmd == commandBuy || cmd == commandSell:
		value = shopRequestJSON{ItemID: request.ItemId, Quantity: request.Quantity}
	case cmd == commandAcceptInvite:
		value = inviteRequestJSON{Token: request.InviteToken}
	case cmd == commandRemoveFriend || cmd == commandAddFriendByUID || cmd == commandRequestFriend:
		value = peerRequestJSON{PeerUID: clientjson.UID(request.PeerUid)}
	case cmd == commandSearchUser:
		value = usernameRequestJSON{Username: request.Username}
	case cmd == commandAcceptFriendRequest || cmd == commandRejectFriendRequest:
		value = fromRequestJSON{FromUID: clientjson.UID(request.FromUid)}
	case cmd == commandPetActivate:
		value = petActivateRequestJSON{DogType: request.DogType}
	case cmd == commandPetFeed:
		value = petFeedRequestJSON{Grams: request.Grams}
	case cmd == commandTaskClaim:
		value = taskRequestJSON{TaskID: request.TaskId}
	case cmd == commandMailRead || cmd == commandMailDelete:
		value = mailRequestJSON{MailID: clientjson.Uint64(request.MailId), All: request.All}
	case cmd == commandMailClaim:
		value = struct {
			MailID clientjson.Uint64 `json:"mail_id"`
		}{MailID: clientjson.Uint64(request.MailId)}
	case cmd == commandSetTimeProfile:
		value = timeProfileRequestJSON{TimeProfile: request.TimeProfile}
	default:
		return nil, fmt.Errorf("wireenv: unsupported command request %d", cmd)
	}
	return json.Marshal(value)
}

type actionResponseJSON struct {
	FarmSeq      clientjson.Uint64        `json:"farm_seq"`
	Patch        farm.PatchJSON           `json:"patch"`
	CodexRewards []farm.CodexRewardNotice `json:"codex_rewards,omitempty"`
}

type visitorRewardJSON struct {
	ReqID        clientjson.Uint64 `json:"req_id"`
	ExpGained    uint32            `json:"exp_gained"`
	CoinGained   clientjson.Int64  `json:"coin_gained"`
	CropID       uint32            `json:"crop_id,omitempty"`
	Amount       uint32            `json:"amount,omitempty"`
	Compensation clientjson.Int64  `json:"compensation,omitempty"`
	DogType      uint32            `json:"dog_type,omitempty"`
}

type friendJSON struct {
	UID          clientjson.UID `json:"uid"`
	Nickname     string         `json:"nickname"`
	HasStealable bool           `json:"has_stealable"`
}

type userJSON struct {
	UID      clientjson.UID `json:"uid"`
	Nickname string         `json:"nickname"`
}

type friendRequestJSON struct {
	FromUID   clientjson.UID `json:"from_uid"`
	Nickname  string         `json:"nickname"`
	CreatedAt int64          `json:"created_at"`
}

func patchToProto(value farm.PatchJSON) *publicv3.FarmPatch {
	result := &publicv3.FarmPatch{Coin: value.Coin, Exp: value.Exp, BagChanges: value.BagChanges, WarehouseChanges: value.WarehouseChanges, FarmSeq: value.FarmSeq}
	if value.Plot != nil {
		index := uint32(value.PlotIndex)
		result.PlotIndex = &index
		result.Plot = plotToProto(*value.Plot)
	}
	if value.Codex != nil {
		result.CodexProgress = codexProgressToProto(*value.Codex)
	}
	return result
}

func patchFromProto(value *publicv3.FarmPatch) farm.PatchJSON {
	if value == nil {
		return farm.PatchJSON{}
	}
	result := farm.PatchJSON{Coin: value.Coin, Exp: value.Exp, BagChanges: value.BagChanges, WarehouseChanges: value.WarehouseChanges, FarmSeq: value.FarmSeq}
	if value.PlotIndex != nil {
		result.PlotIndex = uint8(*value.PlotIndex)
	}
	if value.Plot != nil {
		plot := plotFromProto(value.Plot)
		result.Plot = &plot
	}
	if value.CodexProgress != nil {
		codex := codexProgressFromProto(value.CodexProgress)
		result.Codex = &codex
	}
	return result
}

func actionResponseToProto(value actionResponseJSON) *publicv3.ActionResponse {
	result := &publicv3.ActionResponse{FarmSeq: uint64(value.FarmSeq), Patch: patchToProto(value.Patch)}
	for _, reward := range value.CodexRewards {
		result.CodexRewards = append(result.CodexRewards, codexRewardToProto(reward))
	}
	return result
}

func actionResponseFromProto(value *publicv3.ActionResponse) actionResponseJSON {
	if value == nil {
		return actionResponseJSON{}
	}
	result := actionResponseJSON{FarmSeq: clientjson.Uint64(value.FarmSeq), Patch: patchFromProto(value.Patch)}
	for _, reward := range value.CodexRewards {
		result.CodexRewards = append(result.CodexRewards, codexRewardFromProto(reward))
	}
	return result
}

// NewActionCommandResponse builds the response used by both Farm gRPC and the
// public WebSocket without an intermediate JSON representation.
func NewActionCommandResponse(farmSeq uint64, patch farm.PatchJSON, rewards []farm.CodexRewardNotice) *publicv3.CommandResponse {
	return &publicv3.CommandResponse{Action: actionResponseToProto(actionResponseJSON{FarmSeq: clientjson.Uint64(farmSeq), Patch: patch, CodexRewards: rewards})}
}

// NewPetCommandResponse maps Farm's domain status directly to the public
// Protobuf contract without an intermediate JSON representation.
func NewPetCommandResponse(status farm.PetStatus) *publicv3.CommandResponse {
	return &publicv3.CommandResponse{PetStatus: petStatusToProto(status)}
}

// NewTaskListCommandResponse builds the typed task list response.
func NewTaskListCommandResponse(tasks []store.Task, resetAt int64) *publicv3.CommandResponse {
	response := &publicv3.CommandResponse{ResetAt: resetAt, Tasks: make([]*publicv3.Task, 0, len(tasks))}
	for _, task := range tasks {
		response.Tasks = append(response.Tasks, taskToProto(task))
	}
	return response
}

// NewTaskRewardCommandResponse builds a typed task or daily-login reward.
func NewTaskRewardCommandResponse(reward store.TaskReward) *publicv3.CommandResponse {
	return &publicv3.CommandResponse{TaskReward: &publicv3.TaskReward{Coin: reward.Coin, Exp: reward.Exp}}
}

// NewMailListCommandResponse builds the typed mailbox response.
func NewMailListCommandResponse(mails []store.Mail) *publicv3.CommandResponse {
	response := &publicv3.CommandResponse{Mails: make([]*publicv3.Mail, 0, len(mails))}
	for _, mail := range mails {
		response.Mails = append(response.Mails, mailToProto(mail))
	}
	return response
}

// NewMailMutationCommandResponse reports a typed read/delete count.
func NewMailMutationCommandResponse(affected int64) *publicv3.CommandResponse {
	return &publicv3.CommandResponse{Affected: affected}
}

// NewMailClaimCommandResponse builds the typed claimed-mail response.
func NewMailClaimCommandResponse(mail store.Mail) *publicv3.CommandResponse {
	return &publicv3.CommandResponse{Mail: mailToProto(mail)}
}

// NewCodexListCommandResponse builds the typed codex list response.
func NewCodexListCommandResponse(entries []farm.CodexProgress, total uint32) *publicv3.CommandResponse {
	response := &publicv3.CommandResponse{CodexTotal: total, CodexEntries: make([]*publicv3.CodexProgress, 0, len(entries))}
	for _, entry := range entries {
		response.CodexEntries = append(response.CodexEntries, codexProgressToProto(entry))
	}
	return response
}

func codexProgressToProto(value farm.CodexProgress) *publicv3.CodexProgress {
	return &publicv3.CodexProgress{CropId: uint32(value.CropID), HarvestCount: value.HarvestCount, Tier: value.Tier, NextTarget: value.NextTarget}
}

func codexProgressFromProto(value *publicv3.CodexProgress) farm.CodexProgress {
	if value == nil {
		return farm.CodexProgress{}
	}
	return farm.CodexProgress{CropID: uint16(value.CropId), HarvestCount: value.HarvestCount, Tier: value.Tier, NextTarget: value.NextTarget}
}

func codexRewardToProto(value farm.CodexRewardNotice) *publicv3.CodexRewardNotice {
	return &publicv3.CodexRewardNotice{CropId: uint32(value.CropID), Tier: value.Tier, Target: value.Target, RewardCoin: value.RewardCoin}
}

func codexRewardFromProto(value *publicv3.CodexRewardNotice) farm.CodexRewardNotice {
	if value == nil {
		return farm.CodexRewardNotice{}
	}
	return farm.CodexRewardNotice{CropID: uint16(value.CropId), Tier: value.Tier, Target: value.Target, RewardCoin: value.RewardCoin}
}

func petStatusToProto(value farm.PetStatus) *publicv3.PetStatus {
	result := &publicv3.PetStatus{ActiveDog: uint32(value.ActiveDog), Owned: uint32(value.Owned), BowlGrams: value.BowlGrams, BowlEmptyAt: value.BowlEmptyAt, MsPerGram: value.MsPerGram, DogLevel: uint32(value.DogLevel), Intercepts: uint32(value.Intercepts), InterceptionPct: uint32(value.InterceptionPct)}
	for _, dog := range value.Dogs {
		result.Dogs = append(result.Dogs, &publicv3.PetDogStatus{DogType: uint32(dog.DogType), Level: uint32(dog.Level), Intercepts: uint32(dog.Intercepts), InterceptionPct: uint32(dog.InterceptionPct)})
	}
	return result
}

func petStatusFromProto(value *publicv3.PetStatus) farm.PetStatus {
	if value == nil {
		return farm.PetStatus{}
	}
	result := farm.PetStatus{ActiveDog: farm.DogType(value.ActiveDog), Owned: uint8(value.Owned), BowlGrams: value.BowlGrams, BowlEmptyAt: value.BowlEmptyAt, MsPerGram: value.MsPerGram, DogLevel: uint8(value.DogLevel), Intercepts: uint16(value.Intercepts), InterceptionPct: uint8(value.InterceptionPct)}
	for _, dog := range value.Dogs {
		result.Dogs = append(result.Dogs, farm.PetDogStatus{DogType: farm.DogType(dog.DogType), Level: uint8(dog.Level), Intercepts: uint16(dog.Intercepts), InterceptionPct: uint8(dog.InterceptionPct)})
	}
	return result
}

func taskToProto(value store.Task) *publicv3.Task {
	return &publicv3.Task{Id: value.ID, DayKey: value.DayKey, Kind: value.Kind, Title: value.Title, Progress: value.Progress, Target: value.Target, RewardCoin: value.RewardCoin, Claimed: value.Claimed}
}

func taskJSONFromProto(value *publicv3.Task) store.Task {
	if value == nil {
		return store.Task{}
	}
	return store.Task{ID: value.Id, DayKey: value.DayKey, Kind: value.Kind, Title: value.Title, Progress: value.Progress, Target: value.Target, RewardCoin: value.RewardCoin, Claimed: value.Claimed}
}

// TaskToProto exposes the typed TaskNotify conversion to Gateway push code.
func TaskToProto(value store.Task) *publicv3.Task { return taskToProto(value) }

func mailToProto(value store.Mail) *publicv3.Mail {
	return &publicv3.Mail{Id: value.ID, Title: value.Title, AttachmentCoin: value.AttachmentCoin, Claimed: value.Claimed, Read: value.Read, CreatedAt: value.CreatedAt}
}

func mailFromProto(value *publicv3.Mail) store.Mail {
	if value == nil {
		return store.Mail{}
	}
	return store.Mail{ID: value.Id, Title: value.Title, AttachmentCoin: value.AttachmentCoin, Claimed: value.Claimed, Read: value.Read, CreatedAt: value.CreatedAt}
}

func CommandResponseFromJSON(cmd uint32, payload json.RawMessage) (*publicv3.CommandResponse, error) {
	result := &publicv3.CommandResponse{}
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = json.RawMessage(`{}`)
	}
	switch {
	case cmd == commandHandshake:
		var value struct {
			UID clientjson.UID `json:"uid"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.Uid = uint64(value.UID)
	case cmd == commandPing:
		var value struct {
			ClientTime int64 `json:"client_time"`
			ServerTime int64 `json:"server_time"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.ClientTime, result.ServerTime = value.ClientTime, value.ServerTime
	case cmd == commandLeaveFarm || cmd == commandAcceptInvite || cmd == commandRemoveFriend || cmd == commandAddFriendByUID || cmd == commandRequestFriend || cmd == commandAcceptFriendRequest || cmd == commandRejectFriendRequest:
		if err := DecodeStrictJSON(payload, &struct{}{}); err != nil {
			return nil, err
		}
	case isPlotCommand(cmd) || cmd == commandBuy || cmd == commandSell:
		if bytes.Contains(payload, []byte(`"req_id"`)) {
			var value visitorRewardJSON
			if err := DecodeStrictJSON(payload, &value); err != nil {
				return nil, err
			}
			result.VisitorReward = &publicv3.VisitorReward{ReqId: uint64(value.ReqID), ExpGained: value.ExpGained, CoinGained: int64(value.CoinGained), CropId: value.CropID, Amount: value.Amount, Compensation: int64(value.Compensation), DogType: value.DogType}
		} else {
			var value actionResponseJSON
			if err := DecodeStrictJSON(payload, &value); err != nil {
				return nil, err
			}
			result.Action = actionResponseToProto(value)
		}
	case cmd == commandSteal:
		var value visitorRewardJSON
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.VisitorReward = &publicv3.VisitorReward{ReqId: uint64(value.ReqID), ExpGained: value.ExpGained, CoinGained: int64(value.CoinGained), CropId: value.CropID, Amount: value.Amount, Compensation: int64(value.Compensation), DogType: value.DogType}
	case cmd == commandFriendList:
		var value struct {
			Friends []friendJSON `json:"friends"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		for _, friend := range value.Friends {
			result.Friends = append(result.Friends, &publicv3.Friend{Uid: uint64(friend.UID), Nickname: friend.Nickname, HasStealable: friend.HasStealable})
		}
	case cmd == commandSearchUser:
		var value struct {
			Users []userJSON `json:"users"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		for _, user := range value.Users {
			result.Users = append(result.Users, &publicv3.User{Uid: uint64(user.UID), Nickname: user.Nickname})
		}
	case cmd == commandListFriendRequests:
		var value struct {
			Requests []friendRequestJSON `json:"requests"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		for _, request := range value.Requests {
			result.FriendRequests = append(result.FriendRequests, &publicv3.FriendRequest{FromUid: uint64(request.FromUID), Nickname: request.Nickname, CreatedAt: request.CreatedAt})
		}
	case cmd == commandGenShareLink:
		var value struct {
			Path string `json:"path"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.Path = value.Path
	case cmd == commandPetStatus || cmd == commandPetActivate || cmd == commandPetFeed:
		var value farm.PetStatus
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.PetStatus = petStatusToProto(value)
	case cmd == commandTaskList:
		var value struct {
			Tasks   []store.Task `json:"tasks"`
			ResetAt int64        `json:"reset_at"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		for _, task := range value.Tasks {
			result.Tasks = append(result.Tasks, taskToProto(task))
		}
		result.ResetAt = value.ResetAt
	case cmd == commandTaskClaim || cmd == commandDailyLoginClaim:
		var value store.TaskReward
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.TaskReward = &publicv3.TaskReward{Coin: value.Coin, Exp: value.Exp}
	case cmd == commandMailList:
		var value struct {
			Mails []store.Mail `json:"mails"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		for _, mail := range value.Mails {
			result.Mails = append(result.Mails, mailToProto(mail))
		}
	case cmd == commandMailRead || cmd == commandMailDelete:
		var value struct {
			Affected int64 `json:"affected"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.Affected = value.Affected
	case cmd == commandMailClaim:
		var value store.Mail
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.Mail = mailToProto(value)
	case cmd == commandCodexList:
		var value struct {
			Entries []farm.CodexProgress `json:"entries"`
			Total   int                  `json:"total"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		for _, entry := range value.Entries {
			result.CodexEntries = append(result.CodexEntries, codexProgressToProto(entry))
		}
		result.CodexTotal = uint32(value.Total)
	case cmd == commandSetTimeProfile:
		var value struct {
			TimeProfile string `json:"time_profile"`
			Mutable     bool   `json:"time_profile_mutable"`
		}
		if err := DecodeStrictJSON(payload, &value); err != nil {
			return nil, err
		}
		result.TimeProfile, result.TimeProfileMutable = value.TimeProfile, value.Mutable
	default:
		return nil, fmt.Errorf("wireenv: unsupported command response %d", cmd)
	}
	return result, nil
}

func CommandResponseToJSON(cmd uint32, response *publicv3.CommandResponse) (json.RawMessage, error) {
	if response == nil {
		return nil, fmt.Errorf("wireenv: nil command response")
	}
	var value any
	switch {
	case cmd == commandHandshake:
		value = struct {
			UID clientjson.UID `json:"uid"`
		}{UID: clientjson.UID(response.Uid)}
	case cmd == commandPing:
		value = struct {
			ClientTime int64 `json:"client_time"`
			ServerTime int64 `json:"server_time"`
		}{response.ClientTime, response.ServerTime}
	case cmd == commandLeaveFarm || cmd == commandAcceptInvite || cmd == commandRemoveFriend || cmd == commandAddFriendByUID || cmd == commandRequestFriend || cmd == commandAcceptFriendRequest || cmd == commandRejectFriendRequest:
		value = struct{}{}
	case isPlotCommand(cmd) || cmd == commandBuy || cmd == commandSell:
		if response.VisitorReward != nil {
			value = visitorRewardFromProto(response.VisitorReward)
		} else if response.Action != nil {
			value = actionResponseFromProto(response.Action)
		} else {
			value = struct{}{}
		}
	case cmd == commandSteal:
		if response.VisitorReward == nil {
			value = struct{}{}
		} else {
			value = visitorRewardFromProto(response.VisitorReward)
		}
	case cmd == commandFriendList:
		friends := make([]friendJSON, 0, len(response.Friends))
		for _, friend := range response.Friends {
			friends = append(friends, friendJSON{UID: clientjson.UID(friend.Uid), Nickname: friend.Nickname, HasStealable: friend.HasStealable})
		}
		value = struct {
			Friends []friendJSON `json:"friends"`
		}{friends}
	case cmd == commandSearchUser:
		users := make([]userJSON, 0, len(response.Users))
		for _, user := range response.Users {
			users = append(users, userJSON{UID: clientjson.UID(user.Uid), Nickname: user.Nickname})
		}
		value = struct {
			Users []userJSON `json:"users"`
		}{users}
	case cmd == commandListFriendRequests:
		requests := make([]friendRequestJSON, 0, len(response.FriendRequests))
		for _, request := range response.FriendRequests {
			requests = append(requests, friendRequestJSON{FromUID: clientjson.UID(request.FromUid), Nickname: request.Nickname, CreatedAt: request.CreatedAt})
		}
		value = struct {
			Requests []friendRequestJSON `json:"requests"`
		}{requests}
	case cmd == commandGenShareLink:
		value = struct {
			Path string `json:"path"`
		}{response.Path}
	case cmd == commandPetStatus || cmd == commandPetActivate || cmd == commandPetFeed:
		if response.PetStatus == nil {
			value = struct{}{}
		} else {
			value = petStatusFromProto(response.PetStatus)
		}
	case cmd == commandTaskList:
		tasks := make([]store.Task, 0, len(response.Tasks))
		for _, task := range response.Tasks {
			tasks = append(tasks, taskJSONFromProto(task))
		}
		value = struct {
			Tasks   []store.Task `json:"tasks"`
			ResetAt int64        `json:"reset_at"`
		}{tasks, response.ResetAt}
	case cmd == commandTaskClaim || cmd == commandDailyLoginClaim:
		if response.TaskReward == nil {
			value = struct{}{}
		} else {
			value = store.TaskReward{Coin: response.TaskReward.Coin, Exp: response.TaskReward.Exp}
		}
	case cmd == commandMailList:
		mails := make([]store.Mail, 0, len(response.Mails))
		for _, mail := range response.Mails {
			mails = append(mails, mailFromProto(mail))
		}
		value = struct {
			Mails []store.Mail `json:"mails"`
		}{mails}
	case cmd == commandMailRead || cmd == commandMailDelete:
		value = struct {
			Affected int64 `json:"affected"`
		}{response.Affected}
	case cmd == commandMailClaim:
		if response.Mail == nil {
			value = struct{}{}
		} else {
			value = mailFromProto(response.Mail)
		}
	case cmd == commandCodexList:
		entries := make([]farm.CodexProgress, 0, len(response.CodexEntries))
		for _, entry := range response.CodexEntries {
			entries = append(entries, codexProgressFromProto(entry))
		}
		value = struct {
			Entries []farm.CodexProgress `json:"entries"`
			Total   uint32               `json:"total"`
		}{entries, response.CodexTotal}
	case cmd == commandSetTimeProfile:
		value = struct {
			TimeProfile string `json:"time_profile"`
			Mutable     bool   `json:"time_profile_mutable"`
		}{response.TimeProfile, response.TimeProfileMutable}
	default:
		return nil, fmt.Errorf("wireenv: unsupported command response %d", cmd)
	}
	return json.Marshal(value)
}

func visitorRewardFromProto(value *publicv3.VisitorReward) visitorRewardJSON {
	if value == nil {
		return visitorRewardJSON{}
	}
	return visitorRewardJSON{ReqID: clientjson.Uint64(value.ReqId), ExpGained: value.ExpGained, CoinGained: clientjson.Int64(value.CoinGained), CropID: value.CropId, Amount: value.Amount, Compensation: clientjson.Int64(value.Compensation), DogType: value.DogType}
}

func NewVisitorRewardCommandResponse(reqID uint64, expGained uint32, coinGained int64, cropID, amount uint32, compensation int64, dogType uint32) *publicv3.CommandResponse {
	return &publicv3.CommandResponse{VisitorReward: &publicv3.VisitorReward{
		ReqId: reqID, ExpGained: expGained, CoinGained: coinGained,
		CropId: cropID, Amount: amount, Compensation: compensation, DogType: dogType,
	}}
}

func PlayerDeltaToProto(value farm.PlayerDelta) *publicv3.PlayerDelta {
	result := &publicv3.PlayerDelta{Coin: value.Coin, Exp: value.Exp, Level: uint32(value.Level), Bag: value.Bag, Warehouse: value.Warehouse}
	if value.Pet != nil {
		result.Pet = petStatusToProto(*value.Pet)
	}
	return result
}

func PlayerDeltaFromProto(value *publicv3.PlayerDelta) farm.PlayerDelta {
	if value == nil {
		return farm.PlayerDelta{}
	}
	result := farm.PlayerDelta{Coin: value.Coin, Exp: value.Exp, Level: uint16(value.Level), Bag: value.Bag, Warehouse: value.Warehouse}
	if value.Pet != nil {
		pet := petStatusFromProto(value.Pet)
		result.Pet = &pet
	}
	return result
}
