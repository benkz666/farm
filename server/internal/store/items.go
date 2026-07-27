package store

import (
	"fmt"
	"strconv"
	"strings"

	"farm/server/internal/farm"
)

// item.kind 与架构 5.8 节一致。
const (
	ItemKindSeed       uint8 = 1
	ItemKindFertilizer uint8 = 2
	ItemKindDogFood    uint8 = 3
	ItemKindFruit      uint8 = 4
)

// ParseItemKey 把聚合内的 ItemKey 解析为 (kind, item_id)。
func ParseItemKey(key farm.ItemKey) (kind uint8, itemID uint16, err error) {
	s := string(key)
	switch {
	case strings.HasPrefix(s, "seed:"):
		kind = ItemKindSeed
		s = strings.TrimPrefix(s, "seed:")
	case strings.HasPrefix(s, "fert:"):
		kind = ItemKindFertilizer
		s = strings.TrimPrefix(s, "fert:")
	case strings.HasPrefix(s, "dogfood:"):
		kind = ItemKindDogFood
		s = strings.TrimPrefix(s, "dogfood:")
	case strings.HasPrefix(s, "fruit:"):
		kind = ItemKindFruit
		s = strings.TrimPrefix(s, "fruit:")
	default:
		return 0, 0, fmt.Errorf("store: unknown item key %q", key)
	}
	id, err := strconv.ParseUint(s, 10, 16)
	if err != nil || id == 0 {
		return 0, 0, fmt.Errorf("store: bad item id in key %q", key)
	}
	if kind == ItemKindDogFood && id != 1 {
		return 0, 0, fmt.Errorf("store: unsupported dog food item id %d", id)
	}
	return kind, uint16(id), nil
}

// FormatItemKey 由 (kind, item_id) 生成聚合 ItemKey。
func FormatItemKey(kind uint8, itemID uint16) (farm.ItemKey, error) {
	if itemID == 0 {
		return "", fmt.Errorf("store: item_id must be non-zero")
	}
	switch kind {
	case ItemKindSeed:
		return farm.SeedItem(itemID), nil
	case ItemKindFertilizer:
		return farm.FertilizerItem(itemID), nil
	case ItemKindDogFood:
		if itemID != 1 {
			return "", fmt.Errorf("store: unsupported dog food item id %d", itemID)
		}
		return farm.DogFoodItem(), nil
	case ItemKindFruit:
		return farm.FruitItem(itemID), nil
	default:
		return "", fmt.Errorf("store: unsupported item kind %d", kind)
	}
}
