package gameconf

import "testing"

func TestWhiteRadishMatchesClientConfig(t *testing.T) {
	crop, ok := CropByID(1)
	if !ok {
		t.Fatalf("CropByID(1) not found, want 白萝卜")
	}
	if crop.ID != 1 {
		t.Fatalf("ID=%d, want 1", crop.ID)
	}
	if crop.SeedPrice != 125 {
		t.Fatalf("SeedPrice=%d, want 125 (与 client bailuobo 对齐)", crop.SeedPrice)
	}
	if crop.CycleHours != 10 {
		t.Fatalf("CycleHours=%d, want 10", crop.CycleHours)
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

func TestCropTableSubset(t *testing.T) {
	// 至少白萝卜 + 2 种作物，便于解锁测试
	want := []uint16{1, 2, 3}
	for _, id := range want {
		if _, ok := CropByID(id); !ok {
			t.Fatalf("CropByID(%d) not found, 子集应至少含 3 种作物", id)
		}
	}
	if _, ok := CropByID(0); ok {
		t.Fatalf("CropByID(0) 应返回 false（0 表示无作物）")
	}
	if _, ok := CropByID(9999); ok {
		t.Fatalf("CropByID(9999) 应返回 false")
	}
}

func TestCarrotAndCabbageUnlock(t *testing.T) {
	carrot, ok := CropByID(2)
	if !ok {
		t.Fatalf("CropByID(2) not found, want 胡萝卜")
	}
	if carrot.UnlockLevel != 0 || carrot.SeedPrice != 163 || carrot.CycleHours != 13 || carrot.Yield != 17 {
		t.Fatalf("胡萝卜配置不符: %+v", carrot)
	}
	cabbage, ok := CropByID(3)
	if !ok {
		t.Fatalf("CropByID(3) not found, want 大白菜")
	}
	if cabbage.UnlockLevel != 1 || cabbage.SeedPrice != 168 || cabbage.CycleHours != 14 || cabbage.Yield != 17 {
		t.Fatalf("大白菜配置不符: %+v", cabbage)
	}
}
