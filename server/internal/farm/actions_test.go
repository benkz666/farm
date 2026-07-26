package farm

import (
	"reflect"
	"testing"

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
		SeasonDuration: 1_000,
		MatureAt:       actionNow + 1_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}
	before := cloneAggregate(agg)

	result := agg.ApplyPlotAction(PlotAction{Kind: Till, PlotIndex: 0}, actionNow)

	if result.Err != pkgerr.PlotNotWasteland {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.PlotNotWasteland)
	}
	if !reflect.DeepEqual(*agg, before) {
		t.Fatalf("illegal action mutated aggregate:\n got %#v\nwant %#v", *agg, &before)
	}
}

func TestApplyPlotActionMatureCareIsNoOp(t *testing.T) {
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

			if result.Err != pkgerr.OK {
				t.Fatalf("Err = %d, want OK", result.Err)
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

func TestApplyPlotActionClearFailureDeductsGrowingHealth(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateGrowing,
		CropID:         1,
		SeasonDuration: 1_000,
		MatureAt:       actionNow + 1_000,
		LastSettleAt:   actionNow,
		LastWaterAt:    actionNow,
	}

	result := agg.ApplyPlotAction(PlotAction{Kind: Clear, PlotIndex: 0}, actionNow)

	if result.Err != pkgerr.PlotNotCleanable {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.PlotNotCleanable)
	}
	if got, want := agg.Plots[0].AccruedWeighted, int64(10_000); got != want {
		t.Fatalf("AccruedWeighted = %d, want %d", got, want)
	}
}

