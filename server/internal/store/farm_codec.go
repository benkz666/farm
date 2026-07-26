package store

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"farm/server/internal/farm"
)

// EncodePlot 与 DecodePlot 是 farm_plot.blob 列的唯一编解码入口（规格 5.2 节：
// 「在 store 包内单一函数编解码，禁止两处各写一套互不兼容的格式」）。
//
// 采用固定顺序小端 binary 编码：先写定长字段（与 farm.Plot 字段声明顺序一致），
// 再写 Stealers 变长部分（uint16 长度前缀 + uint32 数组）。

func EncodePlot(p farm.Plot) ([]byte, error) {
	buf := new(bytes.Buffer)

	fixed := []any{
		p.State, p.SeasonIndex, p.SeasonTotal, p.StageCount,
		p.FertMask, p.WeedNextWin, p.PestNextWin,
		p.CropID, p.FinalYield, p.StolenCount,
		p.PlantNonce, p.HarvestRound,
		p.SeasonStartAt, p.SeasonDuration, p.MatureAt, p.LastSettleAt,
		p.LastWaterAt, p.WeedSince, p.PestSince, p.AccruedWeighted,
	}
	for _, f := range fixed {
		if err := binary.Write(buf, binary.LittleEndian, f); err != nil {
			return nil, fmt.Errorf("store: encode plot field %T: %w", f, err)
		}
	}

	if len(p.Stealers) > 0xFFFF {
		return nil, fmt.Errorf("store: encode plot: too many stealers (%d)", len(p.Stealers))
	}
	if err := binary.Write(buf, binary.LittleEndian, uint16(len(p.Stealers))); err != nil {
		return nil, fmt.Errorf("store: encode plot stealers count: %w", err)
	}
	for _, uid := range p.Stealers {
		if err := binary.Write(buf, binary.LittleEndian, uid); err != nil {
			return nil, fmt.Errorf("store: encode plot stealer uid: %w", err)
		}
	}

	return buf.Bytes(), nil
}

func DecodePlot(blob []byte) (farm.Plot, error) {
	var p farm.Plot
	r := bytes.NewReader(blob)

	fixed := []any{
		&p.State, &p.SeasonIndex, &p.SeasonTotal, &p.StageCount,
		&p.FertMask, &p.WeedNextWin, &p.PestNextWin,
		&p.CropID, &p.FinalYield, &p.StolenCount,
		&p.PlantNonce, &p.HarvestRound,
		&p.SeasonStartAt, &p.SeasonDuration, &p.MatureAt, &p.LastSettleAt,
		&p.LastWaterAt, &p.WeedSince, &p.PestSince, &p.AccruedWeighted,
	}
	for _, f := range fixed {
		if err := binary.Read(r, binary.LittleEndian, f); err != nil {
			return farm.Plot{}, fmt.Errorf("store: decode plot field %T: %w", f, err)
		}
	}

	var stealerCount uint16
	if err := binary.Read(r, binary.LittleEndian, &stealerCount); err != nil {
		return farm.Plot{}, fmt.Errorf("store: decode plot stealers count: %w", err)
	}
	if stealerCount > 0 {
		p.Stealers = make([]uint32, stealerCount)
		for i := range p.Stealers {
			if err := binary.Read(r, binary.LittleEndian, &p.Stealers[i]); err != nil {
				return farm.Plot{}, fmt.Errorf("store: decode plot stealer uid: %w", err)
			}
		}
	}

	return p, nil
}
