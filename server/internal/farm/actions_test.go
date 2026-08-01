package farm

import (
	"reflect"
	"testing"
	"time"

	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

const actionNow int64 = 10_000

func TestApplyPlotActionMainPath(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Items[SeedItem(1)] = 1

	tests := []struct {
		name    string
		action  PlotAction
		wantErr pkgerr.Code
		check   func(*testing.T, *Aggregate)
	}{
		{
			name:   "荒地锄地变为空闲已翻",
			action: PlotAction{Kind: Till, PlotIndex: 0},
			check: func(t *testing.T, got *Aggregate) {
				t.Helper()
				if got.Plots[0].State != StateTilled {
					t.Fatalf("State = %d, want StateTilled", got.Plots[0].State)
				}
			},
		},
		{
			name:   "空闲地播种消耗背包种子",
			action: PlotAction{Kind: Plant, PlotIndex: 0, Arg: 1},
			check: func(t *testing.T, got *Aggregate) {
				t.Helper()
				p := got.Plots[0]
				if p.State != StateGrowing || p.CropID != 1 {
					t.Fatalf("plot = %#v, want growing white radish", p)
				}
				if p.SeasonDuration != 10*gameconf.HourMs(gameconf.TimeProfileDemo) {
					t.Fatalf("SeasonDuration = %d, want demo-scaled 10 hours", p.SeasonDuration)
				}
				if got.Items[SeedItem(1)] != 0 {
					t.Fatalf("white radish seed count = %d, want 0", got.Items[SeedItem(1)])
				}
				// 本用例只验证浇水保满产；跳过草/虫窗口，避免概率事件干扰产量断言。
				got.Plots[0].WeedNextWin = gameconf.RiskWindowsPerSeason
				got.Plots[0].PestNextWin = gameconf.RiskWindowsPerSeason
			},
		},
		{
			name:   "生长中首次补水",
			action: PlotAction{Kind: Water, PlotIndex: 0},
			check: func(t *testing.T, got *Aggregate) {
				t.Helper()
				if got.Plots[0].State != StateGrowing {
					t.Fatalf("State = %d, want StateGrowing", got.Plots[0].State)
				}
			},
		},
		{
			name:   "生长中二次补水保满产",
			action: PlotAction{Kind: Water, PlotIndex: 0},
			check: func(t *testing.T, got *Aggregate) {
				t.Helper()
				if got.Plots[0].State != StateGrowing {
					t.Fatalf("State = %d, want StateGrowing", got.Plots[0].State)
				}
			},
		},
		{
			name:   "成熟作物收获后进入残茬并入仓",
			action: PlotAction{Kind: Harvest, PlotIndex: 0},
			check: func(t *testing.T, got *Aggregate) {
				t.Helper()
				if got.Plots[0].State != StateResidue {
					t.Fatalf("State = %d, want StateResidue", got.Plots[0].State)
				}
				if got.Items[FruitItem(1)] != 16 {
					t.Fatalf("white radish fruit count = %d, want 16", got.Items[FruitItem(1)])
				}
			},
		},
		{
			name:   "清理残茬回到空闲地",
			action: PlotAction{Kind: Clear, PlotIndex: 0},
			check: func(t *testing.T, got *Aggregate) {
				t.Helper()
				if got.Plots[0].State != StateTilled {
					t.Fatalf("State = %d, want StateTilled", got.Plots[0].State)
				}
			},
		},
	}

	waterStep := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := actionNow
			switch tt.action.Kind {
			case Water:
				// 水分窗 = 本季 35%。在窗边界各浇一次，避免缺水累计。
				waterStep++
				now += int64(waterStep) * 21_000
			case Harvest:
				now += 10 * gameconf.HourMs(gameconf.TimeProfileDemo)
			}
			result := agg.ApplyPlotAction(tt.action, now)
			if result.Err != tt.wantErr {
				t.Fatalf("Err = %d, want %d", result.Err, tt.wantErr)
			}
			tt.check(t, agg)
		})
	}
}

func TestApplyPlotActionIllegalStateDoesNotMutateAggregate(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		SeasonDuration: 60_000,
		MatureAt:       actionNow + 60_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}
	before := cloneAggregate(agg)

	// now 推过水分窗：若错误地把 advance 写回，AccruedWeighted / LastSettleAt 会变。
	result := agg.ApplyPlotAction(PlotAction{Kind: Till, PlotIndex: 0}, actionNow+30_000)

	if result.Err != pkgerr.PlotNotWasteland {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.PlotNotWasteland)
	}
	if !reflect.DeepEqual(*agg, before) {
		t.Fatalf("illegal action mutated aggregate:\n got %#v\nwant %#v", *agg, &before)
	}
}

func TestApplyPlotActionFailureDoesNotExpireCrossReservation(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Coin = 830
	agg.CrossPending = map[uint64]CrossReservation{
		7: {ReqID: 7, OwnerUID: 2, Steal: true, FrozenCoin: 170, ReservedAt: actionNow - CrossPendingTimeout},
	}
	before := cloneAggregate(agg)

	result := agg.ApplyPlotAction(PlotAction{Kind: Plant, PlotIndex: 0, Arg: 1}, actionNow)

	if result.Err != pkgerr.PlotNotTilled {
		t.Fatalf("Err = %d, want PlotNotTilled", result.Err)
	}
	if agg.Coin != before.Coin || len(agg.CrossPending) != 1 {
		t.Fatalf("failed action changed cross state: coin=%d pending=%#v", agg.Coin, agg.CrossPending)
	}
}

func TestApplyPlotActionPlantOnWastelandRejected(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Items[SeedItem(1)] = 1
	before := cloneAggregate(agg)

	result := agg.ApplyPlotAction(PlotAction{Kind: Plant, PlotIndex: 0, Arg: 1}, actionNow)

	if result.Err != pkgerr.PlotNotTilled {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.PlotNotTilled)
	}
	if !reflect.DeepEqual(*agg, before) {
		t.Fatalf("illegal plant mutated aggregate")
	}
}

func TestApplyPlotActionWeedAndPestClearHazards(t *testing.T) {
	cfg := NewAdvanceConfig(mustCrop(1))
	const elapsed int64 = 1_000

	t.Run("Weed", func(t *testing.T) {
		agg := NewAggregate(1, "alice")
		agg.Plots[0] = Plot{
			State:          StateGrowing,
			CropID:         1,
			SeasonDuration: 60_000,
			MatureAt:       actionNow + 60_000,
			LastSettleAt:   actionNow,
			LastWaterAt:    actionNow,
			WeedSince:      actionNow,
		}

		result := agg.ApplyPlotAction(PlotAction{Kind: Weed, PlotIndex: 0}, actionNow+elapsed)
		if result.Err != pkgerr.OK {
			t.Fatalf("Err = %d, want OK", result.Err)
		}
		p := agg.Plots[0]
		if p.WeedSince != 0 {
			t.Fatalf("WeedSince = %d, want 0", p.WeedSince)
		}
		want := cfg.WeedWeight * elapsed
		if p.AccruedWeighted != want {
			t.Fatalf("AccruedWeighted = %d, want %d", p.AccruedWeighted, want)
		}
	})

	t.Run("Pest", func(t *testing.T) {
		agg := NewAggregate(1, "alice")
		agg.Plots[0] = Plot{
			State:          StateGrowing,
			CropID:         1,
			SeasonDuration: 60_000,
			MatureAt:       actionNow + 60_000,
			LastSettleAt:   actionNow,
			LastWaterAt:    actionNow,
			PestSince:      actionNow,
		}

		result := agg.ApplyPlotAction(PlotAction{Kind: Pest, PlotIndex: 0}, actionNow+elapsed)
		if result.Err != pkgerr.OK {
			t.Fatalf("Err = %d, want OK", result.Err)
		}
		p := agg.Plots[0]
		if p.PestSince != 0 {
			t.Fatalf("PestSince = %d, want 0", p.PestSince)
		}
		want := cfg.PestWeight * elapsed
		if p.AccruedWeighted != want {
			t.Fatalf("AccruedWeighted = %d, want %d", p.AccruedWeighted, want)
		}
	})
}

func TestApplyPlotActionMatureCareIsRejectedWithoutMutation(t *testing.T) {
	for _, kind := range []PlotActionKind{Water, Weed, Pest} {
		t.Run(kind.String(), func(t *testing.T) {
			agg := NewAggregate(1, "alice")
			agg.Plots[0] = Plot{
				State:          StateMature,
				CropID:         1,
				SeasonDuration: 10 * gameconf.HourMs(gameconf.TimeProfileDemo),
				MatureAt:       actionNow,
				FinalYield:     16,
			}
			before := cloneAggregate(agg)

			result := agg.ApplyPlotAction(PlotAction{Kind: kind, PlotIndex: 0}, actionNow)

			if result.Err != pkgerr.PlotNotGrowing {
				t.Fatalf("Err = %d, want PlotNotGrowing", result.Err)
			}
			if !reflect.DeepEqual(*agg, before) {
				t.Fatalf("mature care action mutated aggregate:\n got %#v\nwant %#v", *agg, &before)
			}
		})
	}
}

func cloneAggregate(a *Aggregate) Aggregate {
	cp := *a
	cp.Items = make(map[ItemKey]uint32, len(a.Items))
	for k, v := range a.Items {
		cp.Items[k] = v
	}
	return cp
}

func TestApplyPlotActionClearGrowingFailsWithoutMutation(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		SeasonDuration: 1_000,
		MatureAt:       actionNow + 1_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}
	before := cloneAggregate(agg)

	result := agg.ApplyPlotAction(PlotAction{Kind: Clear, PlotIndex: 0}, actionNow)

	if result.Err != pkgerr.PlotNotCleanable {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.PlotNotCleanable)
	}
	if !reflect.DeepEqual(*agg, before) {
		t.Fatalf("failed clear mutated aggregate:\n got %#v\nwant %#v", *agg, &before)
	}
}

func TestApplyPlotActionUprootGrowingReturnsTilledWithoutReward(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Exp = 120
	agg.RecalcLevel()
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		SeasonDuration: 1_000,
		MatureAt:       actionNow + 1_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}
	seqBefore := agg.FarmSeq
	expBefore := agg.Exp

	result := agg.ApplyPlotAction(PlotAction{
		Kind:      Clear,
		PlotIndex: 0,
		Arg:       ClearArgUproot,
	}, actionNow)

	if result.Err != pkgerr.OK {
		t.Fatalf("Uproot Err = %d, want OK", result.Err)
	}
	if got := agg.Plots[0]; got.State != StateTilled || got.CropID != 0 {
		t.Fatalf("uprooted plot = %#v, want clean tilled plot", got)
	}
	if agg.Exp != expBefore || agg.FarmSeq != seqBefore+1 {
		t.Fatalf("uproot side effects exp=%d seq=%d, want exp=%d seq=%d", agg.Exp, agg.FarmSeq, expBefore, seqBefore+1)
	}
}

func TestPlotActionPermissionMatrixMatchesDesign(t *testing.T) {
	want := map[uint8]map[PlotActionKind]bool{
		StateWasteland: {Till: true},
		StateTilled:    {Plant: true},
		StateGrowing:   {Water: true, Weed: true, Pest: true, Fertilize: true},
		StateMature:    {Harvest: true, Steal: true},
		StateResidue:   {Clear: true},
		StateWithered:  {Clear: true},
	}
	for state := uint8(0); state < StateCount; state++ {
		for action := Till; action <= Steal; action++ {
			if got, expected := AllowsPlotAction(state, action), want[state][action]; got != expected {
				t.Fatalf("state=%d action=%s allowed=%t, want %t", state, action, got, expected)
			}
		}
	}
}

func TestSuccessfulTillAndClearAwardEligibleHiddenSeeds(t *testing.T) {
	originalRandom := hiddenSeedRandom
	t.Cleanup(func() { hiddenSeedRandom = originalRandom })

	rolls := []uint32{0, 0, 0, 1}
	hiddenSeedRandom = func(limit uint32) (uint32, error) {
		if len(rolls) == 0 {
			t.Fatalf("unexpected random roll with limit %d", limit)
		}
		got := rolls[0]
		rolls = rolls[1:]
		if got >= limit {
			t.Fatalf("roll %d outside limit %d", got, limit)
		}
		return got, nil
	}

	agg := NewAggregate(1, "alice")
	agg.Exp = 4_000 // Lv.20；成功动作会重新按经验计算等级。
	agg.RecalcLevel()
	if result := agg.ApplyPlotAction(PlotAction{Kind: Till, PlotIndex: 0}, actionNow); result.Err != pkgerr.OK {
		t.Fatalf("Till Err = %d, want OK", result.Err)
	}
	if got := agg.Items[SeedItem(27)]; got != 1 {
		t.Fatalf("ginseng seeds = %d, want 1", got)
	}

	agg.Plots[0] = Plot{State: StateResidue, CropID: 1}
	if result := agg.ApplyPlotAction(PlotAction{Kind: Clear, PlotIndex: 0}, actionNow); result.Err != pkgerr.OK {
		t.Fatalf("Clear Err = %d, want OK", result.Err)
	}
	if got := agg.Items[SeedItem(28)]; got != 1 {
		t.Fatalf("lingzhi seeds = %d, want 1", got)
	}
}

func TestHiddenSeedDropRespectsChanceAndLevel(t *testing.T) {
	originalRandom := hiddenSeedRandom
	t.Cleanup(func() { hiddenSeedRandom = originalRandom })

	t.Run("miss", func(t *testing.T) {
		hiddenSeedRandom = func(limit uint32) (uint32, error) {
			if limit != hiddenSeedDropChanceDenominator {
				t.Fatalf("limit = %d, want chance denominator", limit)
			}
			return hiddenSeedDropThreshold, nil
		}
		agg := NewAggregate(1, "alice")
		agg.grantHiddenSeed()
		if len(agg.Items) != 0 {
			t.Fatalf("items = %#v, want no drop", agg.Items)
		}
	})

	t.Run("only eligible hidden crop", func(t *testing.T) {
		rolls := []uint32{0, 0}
		hiddenSeedRandom = func(limit uint32) (uint32, error) {
			got := rolls[0]
			rolls = rolls[1:]
			if got >= limit {
				t.Fatalf("roll %d outside limit %d", got, limit)
			}
			return got, nil
		}
		agg := NewAggregate(1, "alice")
		agg.Level = 9
		agg.grantHiddenSeed()
		if got := agg.Items[SeedItem(27)]; got != 1 || len(agg.Items) != 1 {
			t.Fatalf("items = %#v, want only ginseng", agg.Items)
		}
	})
}

func TestApplyPlotActionFertilizeMovesMatureAtWithoutChangingSeasonDuration(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Items[FertilizerItem(1)] = 1
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		StageCount:     3,
		SeasonDuration: 60_000,
		MatureAt:       actionNow + 60_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}

	result := agg.ApplyPlotAction(PlotAction{Kind: Fertilize, PlotIndex: 0, Arg: 1}, actionNow+1_000)

	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	got := agg.Plots[0]
	if got.MatureAt != actionNow+54_000 {
		t.Fatalf("MatureAt = %d, want %d", got.MatureAt, actionNow+54_000)
	}
	if got.SeasonDuration != 60_000 {
		t.Fatalf("SeasonDuration = %d, want 60000", got.SeasonDuration)
	}
	if got.FertMask != 0b001 {
		t.Fatalf("FertMask = %03b, want 001", got.FertMask)
	}
	if agg.Items[FertilizerItem(1)] != 0 {
		t.Fatalf("fertilizer count = %d, want 0", agg.Items[FertilizerItem(1)])
	}
}

func TestApplyPlotActionUsesAuthoritativeTimeProfile(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Level = 6
	agg.Items[SeedItem(10)] = 1
	agg.Plots[0].State = StateTilled

	result := agg.ApplyPlotAction(PlotAction{
		Kind:        Plant,
		PlotIndex:   0,
		Arg:         10,
		TimeProfile: gameconf.TimeProfileAuthentic,
	}, actionNow)
	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	want := int64(17 * time.Hour / time.Millisecond)
	if got := agg.Plots[0].SeasonDuration; got != want {
		t.Fatalf("SeasonDuration = %d, want authentic tomato duration %d", got, want)
	}
	if got := agg.Plots[0].MatureAt; got != actionNow+want {
		t.Fatalf("MatureAt = %d, want %d", got, actionNow+want)
	}
}

func TestFertilizerUsesHotSwitchedCurrentSeasonTimeProfile(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Items[FertilizerItem(1)] = 1
	demoDuration := int64(10 * gameconf.HourMs(gameconf.TimeProfileDemo))
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		StageCount:     3,
		SeasonDuration: demoDuration,
		MatureAt:       actionNow + demoDuration,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}

	result := agg.ApplyPlotAction(PlotAction{
		Kind:        Fertilize,
		PlotIndex:   0,
		Arg:         1,
		TimeProfile: gameconf.TimeProfileAuthentic,
	}, actionNow)
	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	authenticDuration := int64(10 * gameconf.HourMs(gameconf.TimeProfileAuthentic))
	if got, want := agg.Plots[0].SeasonDuration, authenticDuration; got != want {
		t.Fatalf("SeasonDuration = %d, want hot-switched duration %d", got, want)
	}
	if got, want := agg.Plots[0].MatureAt, actionNow+authenticDuration-gameconf.HourMs(gameconf.TimeProfileAuthentic); got != want {
		t.Fatalf("MatureAt = %d, want hot-switched fertilizer result %d", got, want)
	}
}

func TestAdvanceAllWithProfileReprofilesGrowingSeasonWithoutResettingProgress(t *testing.T) {
	const now int64 = 1_700_000_000_000
	demoDuration := int64(10 * gameconf.HourMs(gameconf.TimeProfileDemo))
	authenticDuration := int64(10 * gameconf.HourMs(gameconf.TimeProfileAuthentic))
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:           StateGrowing,
		CropID:          1,
		StageCount:      3,
		SeasonStartAt:   now - demoDuration/2,
		SeasonDuration:  demoDuration,
		MatureAt:        now + demoDuration/2,
		LastSettleAt:    now,
		LastWaterAt:     now - demoDuration/4,
		WeedSince:       now - demoDuration/12,
		AccruedWeighted: demoDuration * 10,
	}

	changes := agg.AdvanceAllWithProfile(now, gameconf.TimeProfileAuthentic)
	if len(changes) != 1 || agg.FarmSeq != 1 {
		t.Fatalf("changes = %#v, FarmSeq = %d, want one reprofile delta", changes, agg.FarmSeq)
	}
	got := agg.Plots[0]
	if got.SeasonDuration != authenticDuration {
		t.Fatalf("SeasonDuration = %d, want %d", got.SeasonDuration, authenticDuration)
	}
	if got.SeasonStartAt != now-authenticDuration/2 || got.MatureAt != now+authenticDuration/2 {
		t.Fatalf("reprofiled timeline = start:%d mature:%d", got.SeasonStartAt, got.MatureAt)
	}
	if got.LastWaterAt != now-authenticDuration/4 ||
		got.WeedSince != now-authenticDuration/12 {
		t.Fatalf("care timestamps = water:%d weed:%d", got.LastWaterAt, got.WeedSince)
	}
	if got.AccruedWeighted != authenticDuration*10 || PlotHealth(got) != 90 {
		t.Fatalf("health = %d accrued = %d, want preserved 90", PlotHealth(got), got.AccruedWeighted)
	}

	back := agg.AdvanceAllWithProfile(now, gameconf.TimeProfileDemo)
	if len(back) != 1 || agg.Plots[0].SeasonDuration != demoDuration {
		t.Fatalf("switch back changes = %#v plot = %#v", back, agg.Plots[0])
	}
	if agg.Plots[0].MatureAt != now+demoDuration/2 || PlotHealth(agg.Plots[0]) != 90 {
		t.Fatalf("switch back did not preserve progress/health: %#v", agg.Plots[0])
	}
}

func TestAdvanceAllWithProfilePreservesFertilizerJumpSeparatelyFromElapsedTime(t *testing.T) {
	const now int64 = 1_700_000_000_000
	demoDuration := int64(10 * gameconf.HourMs(gameconf.TimeProfileDemo))
	authenticDuration := int64(10 * gameconf.HourMs(gameconf.TimeProfileAuthentic))
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		StageCount:     3,
		FertMask:       1,
		SeasonStartAt:  now - demoDuration/6,
		SeasonDuration: demoDuration,
		// 已自然生长 1/6，并通过施肥再推进 1/6，合计完成 1/3。
		MatureAt:     now + demoDuration*2/3,
		LastSettleAt: now,
		LastWaterAt:  now - demoDuration/12,
	}

	changes := agg.AdvanceAllWithProfile(now, gameconf.TimeProfileAuthentic)
	if len(changes) != 1 {
		t.Fatalf("changes = %#v, want one reprofile delta", changes)
	}
	got := agg.Plots[0]
	if got.SeasonStartAt != now-authenticDuration/6 {
		t.Fatalf("SeasonStartAt = %d, want elapsed-time mapping %d", got.SeasonStartAt, now-authenticDuration/6)
	}
	if got.MatureAt != now+authenticDuration*2/3 {
		t.Fatalf("MatureAt = %d, want fertilizer-preserving %d", got.MatureAt, now+authenticDuration*2/3)
	}
	if got.FertMask != 1 {
		t.Fatalf("FertMask = %03b, want preserved 001", got.FertMask)
	}
}

func TestApplyPlotActionFertilizeRejectsSameStageWithoutMutation(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Items[FertilizerItem(1)] = 1
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		StageCount:     3,
		FertMask:       0b001,
		SeasonDuration: 60_000,
		MatureAt:       actionNow + 60_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}
	before := cloneAggregate(agg)

	result := agg.ApplyPlotAction(PlotAction{Kind: Fertilize, PlotIndex: 0, Arg: 1}, actionNow+1_000)

	if result.Err != pkgerr.StageAlreadyFertilized {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.StageAlreadyFertilized)
	}
	if !reflect.DeepEqual(*agg, before) {
		t.Fatal("duplicate fertilize mutated aggregate")
	}
}

func TestApplyPlotActionFertilizeMaturesImmediatelyAtLastStageBoundary(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Items[FertilizerItem(1)] = 1
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		StageCount:     4,
		FertMask:       0b0111,
		SeasonStartAt:  actionNow - 55_000,
		SeasonDuration: 60_000,
		MatureAt:       actionNow + 5_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}

	result := agg.ApplyPlotAction(PlotAction{Kind: Fertilize, PlotIndex: 0, Arg: 1}, actionNow)

	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	got := agg.Plots[0]
	if got.State != StateMature {
		t.Fatalf("State = %d, want Mature", got.State)
	}
	if got.MatureAt != actionNow {
		t.Fatalf("MatureAt = %d, want %d", got.MatureAt, actionNow)
	}
	if got.FinalYield == 0 {
		t.Fatal("FinalYield = 0, want maturity settlement")
	}
	if got.HarvestRound != 1 {
		t.Fatalf("HarvestRound = %d, want 1", got.HarvestRound)
	}
	if got.FertMask != 0b1111 {
		t.Fatalf("FertMask = %04b, want 1111", got.FertMask)
	}
	if result.Patch.Plot.State != StateMature {
		t.Fatalf("patch State = %d, want Mature", result.Patch.Plot.State)
	}
}

func TestAdvanceAllVisibleChangeIncrementsFarmSeqAndReturnsPlotChange(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		SeasonDuration: 1_000,
		MatureAt:       actionNow,
		LastSettleAt:   actionNow - 1_000,
		LastWaterAt:    actionNow - 1_000,
	}

	changes := agg.AdvanceAll(actionNow)

	if agg.FarmSeq != 1 {
		t.Fatalf("FarmSeq = %d, want 1", agg.FarmSeq)
	}
	if len(changes) != 1 || changes[0].Index != 0 || changes[0].State != StateMature {
		t.Fatalf("changes = %#v, want mature plot 0", changes)
	}
}

func TestAdvanceAllKeepsMaturePlotStableWithoutDelta(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.FarmSeq = 7
	agg.Plots[0] = Plot{
		State:          StateMature,
		CropID:         1,
		SeasonDuration: 1_000,
		MatureAt:       actionNow,
		LastSettleAt:   actionNow,
		FinalYield:     14,
		HarvestRound:   1,
	}
	before := cloneAggregate(agg)

	changes := agg.AdvanceAll(actionNow + 1_000_000)

	if len(changes) != 0 {
		t.Fatalf("changes = %#v, want none", changes)
	}
	if !reflect.DeepEqual(*agg, before) {
		t.Fatalf("mature aggregate changed over time:\n got %#v\nwant %#v", *agg, before)
	}
}

func TestApplyPlotActionHarvestMovesMultiSeasonCropToNextGrowingSeason(t *testing.T) {
	agg := NewAggregate(1, "alice")
	const appleID uint16 = 4 // 历史持久化 numeric ID：苹果
	agg.Plots[0] = Plot{
		State:           StateMature,
		CropID:          appleID,
		SeasonIndex:     0,
		SeasonTotal:     2,
		StageCount:      4,
		FertMask:        0b1111,
		FinalYield:      23,
		StolenCount:     2,
		SeasonDuration:  120_000,
		MatureAt:        actionNow,
		LastSettleAt:    actionNow,
		LastWaterAt:     actionNow,
		AccruedWeighted: 9_999,
		WeedSince:       actionNow - 1,
		PestSince:       actionNow - 1,
		WeedNextWin:     3,
		PestNextWin:     4,
		HarvestRound:    1,
	}
	now := actionNow + 5_000

	result := agg.ApplyPlotAction(PlotAction{Kind: Harvest, PlotIndex: 0}, now)

	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	got := agg.Plots[0]
	if got.State != StateGrowing || got.SeasonIndex != 1 || got.SeasonTotal != 2 {
		t.Fatalf("plot = %#v, want second growing season", got)
	}
	if got.SeasonStartAt != now || got.MatureAt != now+60_000 || got.SeasonDuration != 60_000 {
		t.Fatalf("next season times = start:%d mature:%d duration:%d", got.SeasonStartAt, got.MatureAt, got.SeasonDuration)
	}
	if got.FertMask != 0 || got.AccruedWeighted != 0 || got.WeedSince != 0 || got.PestSince != 0 || got.LastWaterAt != now {
		t.Fatalf("next season fields not reset: %#v", got)
	}
	if agg.Items[FruitItem(appleID)] != 21 {
		t.Fatalf("apple harvest = %d, want 21", agg.Items[FruitItem(appleID)])
	}
}
