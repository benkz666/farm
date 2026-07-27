package store_test

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/store"
)

// TestEncodeDecodePlot_Wasteland 覆盖 18 块荒地的 round-trip：
// 编码后再解码，字段应逐一保持一致（含 State/CropID 及其余置零字段）。
func TestEncodeDecodePlot_Wasteland(t *testing.T) {
	for i := 0; i < gameconf.MaxPlots; i++ {
		p := farm.NewWastelandPlot()

		blob, err := store.EncodePlot(p)
		if err != nil {
			t.Fatalf("plot[%d] encode error: %v", i, err)
		}

		got, err := store.DecodePlot(blob)
		if err != nil {
			t.Fatalf("plot[%d] decode error: %v", i, err)
		}

		if got.State != farm.StateWasteland {
			t.Fatalf("plot[%d] want State=%d got %d", i, farm.StateWasteland, got.State)
		}
		if got.CropID != 0 {
			t.Fatalf("plot[%d] want CropID=0 got %d", i, got.CropID)
		}
		if len(got.Stealers) != 0 {
			t.Fatalf("plot[%d] want Stealers empty got %v", i, got.Stealers)
		}
		if got.SeasonStartAt != 0 || got.MatureAt != 0 || got.AccruedWeighted != 0 {
			t.Fatalf("plot[%d] want all reserved fields zero, got %+v", i, got)
		}
	}
}

// TestEncodeDecodePlot_NonZeroFields 覆盖非零字段（含 Stealers 变长切片）的 round-trip，
// 确保 codec 不仅对全零荒地生效，也能正确处理期 2 将写入的完整字段。
func TestEncodeDecodePlot_NonZeroFields(t *testing.T) {
	want := farm.Plot{
		State:           farm.StateGrowing,
		SeasonIndex:     1,
		SeasonTotal:     2,
		StageCount:      4,
		FertMask:        0b0101,
		WeedNextWin:     3,
		PestNextWin:     2,
		CropID:          1001,
		FinalYield:      0,
		StolenCount:     5,
		PlantNonce:      123456789,
		HarvestRound:    7,
		SeasonStartAt:   1_700_000_000_000,
		SeasonDuration:  36_000_000,
		MatureAt:        1_700_036_000_000,
		LastSettleAt:    1_700_010_000_000,
		LastWaterAt:     1_700_005_000_000,
		WeedSince:       1_700_020_000_000,
		PestSince:       0,
		AccruedWeighted: 42,
		Stealers:        []uint64{7, 42, 1<<40 | 100},
	}

	blob, err := store.EncodePlot(want)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	if len(blob) > 256 {
		t.Fatalf("blob exceeds farm_plot.blob VARBINARY(256) capacity: %d bytes", len(blob))
	}

	got, err := store.DecodePlot(blob)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if got.State != want.State || got.CropID != want.CropID || got.StolenCount != want.StolenCount {
		t.Fatalf("round-trip mismatch: want %+v got %+v", want, got)
	}
	if len(got.Stealers) != len(want.Stealers) {
		t.Fatalf("stealers length mismatch: want %v got %v", want.Stealers, got.Stealers)
	}
	for i := range want.Stealers {
		if got.Stealers[i] != want.Stealers[i] {
			t.Fatalf("stealers[%d] mismatch: want %d got %d", i, want.Stealers[i], got.Stealers[i])
		}
	}
}

func TestDecodePlotAcceptsLegacyUint32Stealers(t *testing.T) {
	legacy := farm.Plot{
		State:          farm.StateMature,
		CropID:         1,
		FinalYield:     16,
		HarvestRound:   3,
		SeasonDuration: 60_000,
		MatureAt:       70_000,
	}
	blob, err := encodeLegacyPlotForTest(legacy, []uint32{7, 42})
	if err != nil {
		t.Fatalf("encode legacy plot: %v", err)
	}

	got, err := store.DecodePlot(blob)
	if err != nil {
		t.Fatalf("DecodePlot legacy blob: %v", err)
	}
	want := []uint64{7, 42}
	if !reflect.DeepEqual(got.Stealers, want) {
		t.Fatalf("legacy Stealers = %#v, want %#v", got.Stealers, want)
	}
}

func encodeLegacyPlotForTest(plot farm.Plot, stealers []uint32) ([]byte, error) {
	plot.Stealers = nil
	blob, err := store.EncodePlot(plot)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint16(blob[len(blob)-2:], uint16(len(stealers)))
	buffer := bytes.NewBuffer(blob)
	for _, uid := range stealers {
		if err := binary.Write(buffer, binary.LittleEndian, uid); err != nil {
			return nil, err
		}
	}
	return buffer.Bytes(), nil
}

// TestDecodePlot_TrailingBytes 确保 blob 末尾残留脏字节时 DecodePlot 失败，避免静默截断。
func TestDecodePlot_TrailingBytes(t *testing.T) {
	blob, err := store.EncodePlot(farm.NewWastelandPlot())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	blob = append(blob, 0xDE, 0xAD)
	if _, err := store.DecodePlot(blob); err == nil {
		t.Fatal("want trailing-bytes error, got nil")
	}
}
