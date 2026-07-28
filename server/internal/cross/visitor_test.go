package cross

import (
	"testing"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

// crop 1 的果实单价是 17，赔付额固定为 170。
const testStealCompensation = 170

func stealAction(reqID uint64) CrossAction {
	return CrossAction{
		ReqID:        reqID,
		Kind:         Steal,
		VisitorUID:   7,
		OwnerUID:     9,
		PlotIndex:    0,
		CropID:       1,
		Compensation: testStealCompensation,
	}
}

func TestReserveVisitorFreezesCompensationFromConfigNotRequest(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	action := stealAction(1)
	// 请求声称的赔付额被刻意放大：访客只应按本地配置冻结，否则解冻时会凭空多退钱。
	action.Compensation = 999_999

	if code := ReserveVisitor(aggregate, VisitorReservation{Action: action}, 1_000); code != pkgerr.OK {
		t.Fatalf("reserve code = %d, want OK", code)
	}
	if aggregate.Coin != gameconf.InitialCoin-testStealCompensation {
		t.Fatalf("coin after freeze = %d, want %d", aggregate.Coin, gameconf.InitialCoin-testStealCompensation)
	}
	reservation, ok := aggregate.CrossPending[1]
	if !ok || reservation.FrozenCoin != testStealCompensation {
		t.Fatalf("reservation = %#v, want frozen %d", reservation, testStealCompensation)
	}
}

func TestReserveVisitorRejectsDuplicateRequestID(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(1)}, 1_000); code != pkgerr.OK {
		t.Fatalf("first reserve code = %d, want OK", code)
	}
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(1)}, 1_000); code != pkgerr.DuplicateOK {
		t.Fatalf("duplicate reserve code = %d, want DuplicateOK", code)
	}
	if aggregate.Coin != gameconf.InitialCoin-testStealCompensation {
		t.Fatalf("duplicate reserve froze twice: coin = %d", aggregate.Coin)
	}
}

func TestReserveVisitorBoundsInFlightReservations(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	aggregate.Coin = 1_000_000

	for reqID := uint64(1); reqID <= 16; reqID++ {
		if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(reqID)}, 1_000); code != pkgerr.OK {
			t.Fatalf("reserve %d code = %d, want OK", reqID, code)
		}
	}
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(17)}, 1_000); code != pkgerr.RateLimited {
		t.Fatalf("reserve beyond bound code = %d, want RateLimited", code)
	}
}

func TestSettleVisitorGrantsFruitAndUnfreezesOnSuccess(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(1)}, 1_000); code != pkgerr.OK {
		t.Fatalf("reserve code = %d, want OK", code)
	}

	reward, delta, code := SettleVisitor(aggregate, CrossResult{
		ReqID: 1, VisitorUID: 7, OwnerUID: 9, Code: pkgerr.OK, CropID: 1, Amount: 3,
	}, 1_100)

	if code != pkgerr.OK || reward.CropID != 1 || reward.Amount != 3 {
		t.Fatalf("reward = %#v code = %d", reward, code)
	}
	if delta == nil {
		t.Fatal("successful steal must report a player delta")
	}
	if aggregate.Coin != gameconf.InitialCoin {
		t.Fatalf("coin after unfreeze = %d, want %d", aggregate.Coin, gameconf.InitialCoin)
	}
	if got := aggregate.Items[farm.FruitItem(1)]; got != 3 {
		t.Fatalf("fruit in bag = %d, want 3", got)
	}
	if len(aggregate.CrossPending) != 0 {
		t.Fatalf("reservation must be consumed, got %#v", aggregate.CrossPending)
	}
}

func TestSettleVisitorIgnoresRepeatedResult(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(1)}, 1_000); code != pkgerr.OK {
		t.Fatalf("reserve code = %d, want OK", code)
	}
	result := CrossResult{ReqID: 1, VisitorUID: 7, OwnerUID: 9, Code: pkgerr.OK, CropID: 1, Amount: 3}
	if _, _, code := SettleVisitor(aggregate, result, 1_100); code != pkgerr.OK {
		t.Fatalf("first settle code = %d, want OK", code)
	}

	// 至少一次投递语义下同一条回执会重放：第二次必须不再发奖。
	_, delta, code := SettleVisitor(aggregate, result, 1_200)

	if code != pkgerr.Timeout {
		t.Fatalf("repeated settle code = %d, want Timeout", code)
	}
	if delta != nil {
		t.Fatal("repeated settle must not report a player delta")
	}
	if got := aggregate.Items[farm.FruitItem(1)]; got != 3 {
		t.Fatalf("fruit granted twice: %d, want 3", got)
	}
	if aggregate.Coin != gameconf.InitialCoin {
		t.Fatalf("coin refunded twice: %d, want %d", aggregate.Coin, gameconf.InitialCoin)
	}
}

func TestSettleVisitorKeepsFrozenCoinWhenIntercepted(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(1)}, 1_000); code != pkgerr.OK {
		t.Fatalf("reserve code = %d, want OK", code)
	}

	reward, _, code := SettleVisitor(aggregate, CrossResult{
		ReqID: 1, VisitorUID: 7, OwnerUID: 9, Code: pkgerr.StealIntercepted, DogType: farm.DogMutt,
	}, 1_100)

	if code != pkgerr.StealIntercepted || reward.Compensation != testStealCompensation {
		t.Fatalf("reward = %#v code = %d", reward, code)
	}
	if aggregate.Coin != gameconf.InitialCoin-testStealCompensation {
		t.Fatalf("intercepted steal must forfeit the freeze, coin = %d", aggregate.Coin)
	}
}

func TestExpiredReservationRefundsWithoutOwnerResult(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: stealAction(1)}, 1_000); code != pkgerr.OK {
		t.Fatalf("reserve code = %d, want OK", code)
	}

	// 主人侧回执永远没有到达（例如 Gateway 进程消失）：下一次触碰聚合就该退钱。
	expired := 1_000 + farm.CrossPendingTimeout
	if !aggregate.ExpireCrossPending(expired) {
		t.Fatal("timed-out reservation must be rolled back")
	}
	if aggregate.Coin != gameconf.InitialCoin {
		t.Fatalf("coin after lazy rollback = %d, want %d", aggregate.Coin, gameconf.InitialCoin)
	}

	// 超时之后迟到的成功回执不能再发奖，否则等于既退款又发货。
	_, _, code := SettleVisitor(aggregate, CrossResult{
		ReqID: 1, VisitorUID: 7, OwnerUID: 9, Code: pkgerr.OK, CropID: 1, Amount: 3,
	}, expired+1)
	if code != pkgerr.Timeout {
		t.Fatalf("late settle code = %d, want Timeout", code)
	}
	if got := aggregate.Items[farm.FruitItem(1)]; got != 0 {
		t.Fatalf("late settle granted fruit: %d", got)
	}
}

func TestMaintenanceReservationRollsBackOnOwnerRejection(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	action := CrossAction{ReqID: 5, Kind: Water, VisitorUID: 7, OwnerUID: 9}
	const dayID uint32 = 12

	if code := ReserveVisitor(aggregate, VisitorReservation{Action: action, DayID: dayID}, 1_000); code != pkgerr.OK {
		t.Fatalf("reserve code = %d, want OK", code)
	}
	if aggregate.Daily.MaintainCnt != 1 {
		t.Fatalf("maintenance count after reserve = %d, want 1", aggregate.Daily.MaintainCnt)
	}

	_, _, code := SettleVisitor(aggregate, CrossResult{
		ReqID: 5, VisitorUID: 7, OwnerUID: 9, Code: pkgerr.AlreadyWatered,
	}, 1_100)

	if code != pkgerr.AlreadyWatered {
		t.Fatalf("settle code = %d, want AlreadyWatered", code)
	}
	if aggregate.Daily.MaintainCnt != 0 {
		t.Fatalf("rejected action must return the reward slot, count = %d", aggregate.Daily.MaintainCnt)
	}
	if aggregate.Exp != 0 {
		t.Fatalf("rejected action granted exp = %d", aggregate.Exp)
	}
}

func TestMaintenanceRollbackAcrossDayBoundaryKeepsTodayCount(t *testing.T) {
	aggregate := farm.NewAggregate(7, "visitor")
	action := CrossAction{ReqID: 5, Kind: Water, VisitorUID: 7, OwnerUID: 9}
	if code := ReserveVisitor(aggregate, VisitorReservation{Action: action, DayID: 12}, 1_000); code != pkgerr.OK {
		t.Fatalf("reserve code = %d, want OK", code)
	}
	// 逻辑日翻页后，昨天那次预占的回滚不能把今天已经攒下的计数清零。
	if !aggregate.ReserveMaintenance(13) {
		t.Fatal("new day reservation must earn a reward")
	}

	aggregate.RollbackMaintenance(12, true)

	if aggregate.Daily.DayID != 13 || aggregate.Daily.MaintainCnt != 1 {
		t.Fatalf("daily state = %#v, want day 13 count 1", aggregate.Daily)
	}
}
