package farm

import "farm/server/platform/pkgerr"

// CrossPendingTimeout 是访客侧预占在聚合内的最长存活时间。
//
// 它比 Gateway 应答客户端用的 5 秒超时长一倍：正常路径下 Gateway 先超时并结算，
// 这里只在 Gateway 进程消失后兜底。两条路径都经 TakeCrossReservation 取记录，
// 谁先取到谁收尾，所以重叠不会导致重复回滚。
const CrossPendingTimeout int64 = 10_000

// maxCrossPending 限制单个访客同时在途的预占数。没有上限时，客户端可以靠连发
// 偷菜请求把全部金币钉在冻结态，或一次性占空当日 150 次维护额度。
const maxCrossPending = 16

// CrossReservation 记录一次跨农场动作在访客侧已经付出的代价。
//
// 它随聚合持久化，因此「谁负责回滚」不再依赖任何进程的内存：主人侧回执、
// Gateway 超时、以及下一次触碰本聚合时的惰性过期，三条路径都从这里取走同一条
// 记录。这也让 FrozenCoin 成为访客自己的权威数据，而不是由调用方在请求里声称的
// 数字——后者可以被用来凭空退款。
type CrossReservation struct {
	ReqID        uint64         `json:"req_id"`
	OwnerUID     uint64         `json:"owner_uid"`
	Steal        bool           `json:"steal,omitempty"`
	MaintainKind PlotActionKind `json:"maintain_kind,omitempty"`
	DayID        uint32         `json:"day_id,omitempty"`
	Rewarded     bool           `json:"rewarded,omitempty"`
	FrozenCoin   int64          `json:"frozen_coin,omitempty"`
	ReservedAt   int64          `json:"reserved_at"`
}

// ReserveCross 扣减本次跨农场动作所需的访客侧资源并登记预占。
// 返回的 CrossReservation 带上了 Rewarded 与 ReservedAt 的实际取值。
func (a *Aggregate) ReserveCross(reservation CrossReservation, now int64) (CrossReservation, pkgerr.Code) {
	if a == nil {
		return reservation, pkgerr.Internal
	}
	if reservation.ReqID == 0 || reservation.OwnerUID == 0 || reservation.OwnerUID == a.UID {
		return reservation, pkgerr.BadRequest
	}

	a.ExpireCrossPending(now)
	if _, exists := a.CrossPending[reservation.ReqID]; exists {
		return reservation, pkgerr.DuplicateOK
	}
	if len(a.CrossPending) >= maxCrossPending {
		return reservation, pkgerr.RateLimited
	}

	reservation.ReservedAt = now
	if reservation.Steal {
		if code := a.FreezeStealCompensation(reservation.FrozenCoin); code != pkgerr.OK {
			return reservation, code
		}
		reservation.Rewarded = false
	} else {
		reservation.FrozenCoin = 0
		reservation.Rewarded = a.ReserveMaintenance(reservation.DayID)
	}

	if a.CrossPending == nil {
		a.CrossPending = make(map[uint64]CrossReservation, 1)
	}
	a.CrossPending[reservation.ReqID] = reservation
	return reservation, pkgerr.OK
}

// TakeCrossReservation 取出并删除一条预占，把收尾责任转交给调用方。
// 找不到说明它已被结算或已被惰性过期回滚，调用方必须当作空操作处理——
// 这正是让重复投递的主人回执天然幂等的地方。
func (a *Aggregate) TakeCrossReservation(reqID, ownerUID uint64) (CrossReservation, bool) {
	if a == nil {
		return CrossReservation{}, false
	}
	reservation, ok := a.CrossPending[reqID]
	if !ok || reservation.OwnerUID != ownerUID {
		return CrossReservation{}, false
	}
	delete(a.CrossPending, reqID)
	if len(a.CrossPending) == 0 {
		a.CrossPending = nil
	}
	return reservation, true
}

// RollbackCross 归还一条预占占用的资源。它幂等于「每条预占只调用一次」，
// 由 TakeCrossReservation 的删除语义保证。
func (a *Aggregate) RollbackCross(reservation CrossReservation) {
	if a == nil {
		return
	}
	if reservation.Steal {
		a.ReleaseStealCompensation(reservation.FrozenCoin)
		return
	}
	a.RollbackMaintenance(reservation.DayID, reservation.Rewarded)
}

// ExpireCrossPending 回滚全部已超时的预占，返回是否发生了变化。
//
// 这是 D1 惰性推进在跨农场资源上的应用：没有后台扫描线程，回滚发生在下一次
// 有人触碰这份聚合的时候。代价是超时的钱要到玩家下次上线才回到账面，收益是
// 任何进程崩溃都不会让它永久失踪。
func (a *Aggregate) ExpireCrossPending(now int64) bool {
	if a == nil || len(a.CrossPending) == 0 {
		return false
	}

	changed := false
	for reqID, reservation := range a.CrossPending {
		if now-reservation.ReservedAt < CrossPendingTimeout {
			continue
		}
		delete(a.CrossPending, reqID)
		a.RollbackCross(reservation)
		changed = true
	}
	if len(a.CrossPending) == 0 {
		a.CrossPending = nil
	}
	return changed
}
