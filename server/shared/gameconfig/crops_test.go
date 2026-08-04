package gameconfig

import (
	"testing"
)

func TestWhiteRadishMatchesClientConfig(t *testing.T) {
	crop, ok := CropByID(1)
	if !ok {
		t.Fatalf("CropByID(1) not found, want 白萝卜")
	}
	if crop.ID != 1 {
		t.Fatalf("ID=%d, want 1", crop.ID)
	}
	if crop.Slug != "bailuobo" {
		t.Fatalf("Slug=%q, want bailuobo", crop.Slug)
	}
	if crop.SeedPrice != 125 {
		t.Fatalf("SeedPrice=%d, want 125 (与设计 bailuobo 对齐)", crop.SeedPrice)
	}
	if crop.CycleMinutes != 600 {
		t.Fatalf("CycleMinutes=%d, want 600 (10h)", crop.CycleMinutes)
	}
	if crop.Yield != 16 {
		t.Fatalf("Yield=%d, want 16", crop.Yield)
	}
	if crop.FruitPrice != 17 {
		t.Fatalf("FruitPrice=%d, want 17", crop.FruitPrice)
	}
	if crop.Seasons != 1 {
		t.Fatalf("Seasons=%d, want 1", crop.Seasons)
	}
	if crop.UnlockLevel != 0 {
		t.Fatalf("UnlockLevel=%d, want 0", crop.UnlockLevel)
	}
	if crop.HarvestExp != 15 {
		t.Fatalf("HarvestExp=%d, want 15", crop.HarvestExp)
	}
}

func TestCropTableHas29UniqueContinuousIDs(t *testing.T) {
	if CropCount != 29 {
		t.Fatalf("CropCount=%d, want 29（设计 26 普通 + 3 隐藏）", CropCount)
	}
	seenSlug := make(map[string]uint16, CropCount)
	for id := uint16(1); id <= CropCount; id++ {
		crop, ok := CropByID(id)
		if !ok {
			t.Fatalf("CropByID(%d) not found, IDs 必须 1..29 连续可查", id)
		}
		if crop.ID != id {
			t.Fatalf("cropTable[%d].ID=%d, want %d（按 numeric ID 索引）", id-1, crop.ID, id)
		}
		if crop.Slug == "" {
			t.Fatalf("CropByID(%d) Slug 为空", id)
		}
		if prev, dup := seenSlug[crop.Slug]; dup {
			t.Fatalf("slug %q 重复：id %d 与 %d", crop.Slug, prev, id)
		}
		seenSlug[crop.Slug] = id
	}
	if _, ok := CropByID(0); ok {
		t.Fatalf("CropByID(0) 应返回 false（0 表示无作物）")
	}
	if _, ok := CropByID(uint16(CropCount + 1)); ok {
		t.Fatalf("CropByID(%d) 应返回 false（越界）", CropCount+1)
	}
	if _, ok := CropByID(9999); ok {
		t.Fatalf("CropByID(9999) 应返回 false")
	}
}

func TestPersistedNumericIDsPreserved(t *testing.T) {
	// 历史服务端已持久化：1–3 普通早期作物，4=苹果。展示顺序与 numeric ID 解耦。
	cases := []struct {
		id   uint16
		slug string
		name string
	}{
		{1, "bailuobo", "白萝卜"},
		{2, "huluobo", "胡萝卜"},
		{3, "dabaicai", "大白菜"},
		{4, "pingguo", "苹果"},
		{15, "xiaomai", "小麦"},
		{26, "hulu", "葫芦"},
		{27, "renshen", "人参"},
		{29, "yaoqianshu", "摇钱树"},
	}
	for _, tt := range cases {
		crop, ok := CropByID(tt.id)
		if !ok {
			t.Fatalf("CropByID(%d) not found, want %s", tt.id, tt.name)
		}
		if crop.Slug != tt.slug {
			t.Fatalf("ID %d Slug=%q, want %q（%s）", tt.id, crop.Slug, tt.slug, tt.name)
		}
		if crop.Name != tt.name {
			t.Fatalf("ID %d Name=%q, want %q", tt.id, crop.Name, tt.name)
		}
	}

	apple, _ := CropByID(4)
	if apple.UnlockLevel != 10 || apple.SeedPrice != 578 || apple.Seasons != 2 || apple.CycleMinutes != 1800 || apple.Yield != 23 {
		t.Fatalf("苹果(ID4) 数值不符设计: %+v", apple)
	}
	if apple.Hidden {
		t.Fatalf("苹果不应为隐藏作物")
	}
	wheat, _ := CropByID(15)
	if wheat.UnlockLevel != 2 || wheat.SeedPrice != 168 || wheat.Seasons != 1 || wheat.CycleMinutes != 840 || wheat.Yield != 18 {
		t.Fatalf("小麦(ID15) 数值不符设计: %+v", wheat)
	}
	ginseng, _ := CropByID(27)
	if !ginseng.Hidden || ginseng.SeedPrice != 0 || ginseng.DropLevel != 0 {
		t.Fatalf("人参应为隐藏且 SeedPrice=0 DropLevel=0: %+v", ginseng)
	}
}

func TestCarrotAndCabbageUnlock(t *testing.T) {
	carrot, ok := CropByID(2)
	if !ok {
		t.Fatalf("CropByID(2) not found, want 胡萝卜")
	}
	if carrot.UnlockLevel != 0 || carrot.SeedPrice != 163 || carrot.CycleMinutes != 780 || carrot.Yield != 17 {
		t.Fatalf("胡萝卜配置不符: %+v", carrot)
	}
	cabbage, ok := CropByID(3)
	if !ok {
		t.Fatalf("CropByID(3) not found, want 大白菜")
	}
	if cabbage.UnlockLevel != 1 || cabbage.SeedPrice != 168 || cabbage.CycleMinutes != 840 || cabbage.Yield != 17 {
		t.Fatalf("大白菜配置不符: %+v", cabbage)
	}
}

func TestSeasonMinutesDesignSplit(t *testing.T) {
	// 策划：后续每季 = 全周期/(季数+1)，首季 = 2×后续；权威单位为整数分钟。
	strawberry, ok := CropByID(16) // 草莓 35h / 2 季
	if !ok {
		t.Fatal("CropByID(16) missing")
	}
	if strawberry.CycleMinutes != 2100 {
		t.Fatalf("strawberry CycleMinutes=%d, want 2100", strawberry.CycleMinutes)
	}
	if got := SeasonMinutes(strawberry, 0); got != 1400 {
		t.Fatalf("strawberry season0=%d, want 1400 (23h20m)", got)
	}
	if got := SeasonMinutes(strawberry, 1); got != 700 {
		t.Fatalf("strawberry season1=%d, want 700 (11h40m)", got)
	}

	orange, ok := CropByID(20) // 橙子 59h / 3 季
	if !ok {
		t.Fatal("CropByID(20) missing")
	}
	if orange.CycleMinutes != 3540 {
		t.Fatalf("orange CycleMinutes=%d, want 3540", orange.CycleMinutes)
	}
	if got := SeasonMinutes(orange, 0); got != 1770 {
		t.Fatalf("orange season0=%d, want 1770 (29h30m)", got)
	}
	if got := SeasonMinutes(orange, 1); got != 885 {
		t.Fatalf("orange season1=%d, want 885 (14h45m)", got)
	}
	if got := SeasonMinutes(orange, 2); got != 885 {
		t.Fatalf("orange season2=%d, want 885", got)
	}

	apple, ok := CropByID(4)
	if !ok {
		t.Fatal("CropByID(4) missing apple")
	}
	if got := SeasonMinutes(apple, 0); got != 1200 {
		t.Fatalf("apple season0=%d, want 1200", got)
	}
	if got := SeasonMinutes(apple, 1); got != 600 {
		t.Fatalf("apple season1=%d, want 600", got)
	}
}

func TestSeasonDurationMsNoTruncationAcrossProfiles(t *testing.T) {
	strawberry, ok := CropByID(16)
	if !ok {
		t.Fatal("missing strawberry")
	}
	for _, profile := range []string{TimeProfileDemo, TimeProfileFast, TimeProfileAuthentic} {
		hour := HourMs(profile)
		if hour%60 != 0 {
			t.Fatalf("HourMs(%s)=%d not divisible by 60", profile, hour)
		}
		want0 := int64(1400) * hour / 60
		want1 := int64(700) * hour / 60
		if got := SeasonDurationMs(strawberry, 0, profile); got != want0 {
			t.Fatalf("profile %s season0=%d, want %d", profile, got, want0)
		}
		if got := SeasonDurationMs(strawberry, 1, profile); got != want1 {
			t.Fatalf("profile %s season1=%d, want %d", profile, got, want1)
		}
	}
}
