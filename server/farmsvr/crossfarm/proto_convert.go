package crossfarm

import (
	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/presence"
)

func actionToProto(action CrossAction) *farmv1.CrossAction {
	return &farmv1.CrossAction{
		ReqId:              action.ReqID,
		Kind:               actionKindToProto(action.Kind),
		VisitorUid:         action.VisitorUID,
		OwnerUid:           action.OwnerUID,
		PlotIndex:          uint32(action.PlotIndex),
		CropId:             uint32(action.CropID),
		Compensation:       action.Compensation,
		FriendshipVerified: action.FriendshipVerified,
		Originator:         connRefToProto(action.Originator),
	}
}

func actionFromProto(action *farmv1.CrossAction) (CrossAction, bool) {
	if action == nil || action.ReqId == 0 {
		return CrossAction{}, false
	}
	kind, ok := actionKindFromProto(action.Kind)
	if !ok {
		return CrossAction{}, false
	}
	return CrossAction{
		ReqID:              action.ReqId,
		Kind:               kind,
		VisitorUID:         action.VisitorUid,
		OwnerUID:           action.OwnerUid,
		PlotIndex:          uint8(action.PlotIndex),
		CropID:             uint16(action.CropId),
		Compensation:       action.Compensation,
		FriendshipVerified: action.FriendshipVerified,
		Originator:         connRefFromProto(action.Originator),
	}, true
}

func connRefToProto(ref presence.ConnRef) *farmv1.ConnRef {
	if ref.ConnID == 0 || ref.GatewayID == "" {
		return nil
	}
	return &farmv1.ConnRef{ConnId: ref.ConnID, GatewayId: ref.GatewayID}
}

func connRefFromProto(ref *farmv1.ConnRef) presence.ConnRef {
	if ref == nil || ref.ConnId == 0 || ref.GatewayId == "" {
		return presence.ConnRef{}
	}
	return presence.ConnRef{ConnID: ref.ConnId, GatewayID: ref.GatewayId}
}

func resultToProto(result CrossResult) *farmv1.CrossResult {
	return &farmv1.CrossResult{
		ReqId:        result.ReqID,
		VisitorUid:   result.VisitorUID,
		OwnerUid:     result.OwnerUID,
		Code:         int32(result.Code),
		CropId:       uint32(result.CropID),
		Amount:       uint32(result.Amount),
		Compensation: result.Compensation,
		DogType:      uint32(result.DogType),
	}
}

func resultFromProto(result *farmv1.CrossResult) (CrossResult, bool) {
	if result == nil || result.ReqId == 0 {
		return CrossResult{}, false
	}
	return CrossResult{
		ReqID:        result.ReqId,
		VisitorUID:   result.VisitorUid,
		OwnerUID:     result.OwnerUid,
		Code:         errcode.Code(result.Code),
		CropID:       uint16(result.CropId),
		Amount:       uint16(result.Amount),
		Compensation: result.Compensation,
		DogType:      farm.DogType(result.DogType),
	}, true
}

func rewardToProto(reward VisitorReward) *farmv1.VisitorReward {
	return &farmv1.VisitorReward{
		ReqId:        reward.ReqID,
		ExpGained:    reward.ExpGained,
		CoinGained:   reward.CoinGained,
		CropId:       uint32(reward.CropID),
		Amount:       uint32(reward.Amount),
		Compensation: reward.Compensation,
		DogType:      uint32(reward.DogType),
	}
}

func rewardFromProto(reward *farmv1.VisitorReward) VisitorReward {
	if reward == nil {
		return VisitorReward{}
	}
	return VisitorReward{
		ReqID:        reward.ReqId,
		ExpGained:    reward.ExpGained,
		CoinGained:   reward.CoinGained,
		CropID:       uint16(reward.CropId),
		Amount:       uint16(reward.Amount),
		Compensation: reward.Compensation,
		DogType:      farm.DogType(reward.DogType),
	}
}

func actionKindToProto(kind ActionKind) farmv1.CrossActionKind {
	switch kind {
	case Water:
		return farmv1.CrossActionKind_CROSS_ACTION_KIND_WATER
	case RemoveWeed:
		return farmv1.CrossActionKind_CROSS_ACTION_KIND_REMOVE_WEED
	case RemovePest:
		return farmv1.CrossActionKind_CROSS_ACTION_KIND_REMOVE_PEST
	case Steal:
		return farmv1.CrossActionKind_CROSS_ACTION_KIND_STEAL
	default:
		return farmv1.CrossActionKind_CROSS_ACTION_KIND_UNSPECIFIED
	}
}

func actionKindFromProto(kind farmv1.CrossActionKind) (ActionKind, bool) {
	switch kind {
	case farmv1.CrossActionKind_CROSS_ACTION_KIND_WATER:
		return Water, true
	case farmv1.CrossActionKind_CROSS_ACTION_KIND_REMOVE_WEED:
		return RemoveWeed, true
	case farmv1.CrossActionKind_CROSS_ACTION_KIND_REMOVE_PEST:
		return RemovePest, true
	case farmv1.CrossActionKind_CROSS_ACTION_KIND_STEAL:
		return Steal, true
	default:
		return "", false
	}
}
