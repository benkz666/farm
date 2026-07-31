package farm

import (
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

// BuyReq 购买商店商品。种子使用 crop_id；化肥使用其 ShopItemID。
type BuyReq struct {
	ItemID   uint16
	Quantity uint32
}

// SellReq 出售仓库果实。期 2 仅支持卖果实：ItemID=crop_id。
type SellReq struct {
	ItemID   uint16
	Quantity uint32
}

// Buy 从商店购买种子或化肥：扣金币、校验解锁、加入背包。
func (a *Aggregate) Buy(req BuyReq) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if req.Quantity == 0 {
		return ActionResult{Err: pkgerr.BadQuantity}
	}
	if req.ItemID == DogFoodShopItemID {
		cost := int64(req.Quantity)
		if a.Coin < cost {
			return ActionResult{Err: pkgerr.NotEnoughCoin}
		}
		a.Coin -= cost
		a.Items[DogFoodItem()] += req.Quantity
		a.FarmSeq++
		return a.okPatch(0)
	}
	if dog, price, unlockLevel, ok := dogPurchaseByItem(req.ItemID); ok {
		if req.Quantity != 1 {
			return ActionResult{Err: pkgerr.BadQuantity}
		}
		if a.Pet.HasDog(dog) {
			return ActionResult{Err: pkgerr.DogAlreadyOwned}
		}
		if a.Level < unlockLevel {
			return ActionResult{Err: pkgerr.DogLocked}
		}
		if a.Coin < price {
			return ActionResult{Err: pkgerr.NotEnoughCoin}
		}
		index, _ := dogIndex(dog)
		a.Coin -= price
		a.Pet.Owned |= 1 << uint(index)
		a.FarmSeq++
		return a.okPatch(0)
	}
	crop, ok := gameconf.CropByID(req.ItemID)
	if ok {
		// 隐藏作物不进商店（种子价 0 / Hidden）；避免免费购入。
		if crop.Hidden || crop.SeedPrice == 0 {
			return ActionResult{Err: pkgerr.ItemNotFound}
		}
		if a.Level < uint16(crop.UnlockLevel) {
			return ActionResult{Err: pkgerr.CropLocked}
		}
		cost := int64(crop.SeedPrice) * int64(req.Quantity)
		if a.Coin < cost {
			return ActionResult{Err: pkgerr.NotEnoughCoin}
		}

		a.Coin -= cost
		key := SeedItem(req.ItemID)
		a.Items[key] += req.Quantity
		a.FarmSeq++
		return a.okPatch(0)
	}
	fertilizer, ok := gameconf.FertilizerByShopItemID(req.ItemID)
	if !ok {
		return ActionResult{Err: pkgerr.ItemNotFound}
	}
	cost := int64(fertilizer.Price) * int64(req.Quantity)
	if a.Coin < cost {
		return ActionResult{Err: pkgerr.NotEnoughCoin}
	}

	a.Coin -= cost
	a.Items[FertilizerItem(fertilizer.ID)] += req.Quantity
	a.FarmSeq++
	return a.okPatch(0)
}

func dogPurchaseByItem(itemID uint16) (dog DogType, price int64, unlockLevel uint16, ok bool) {
	switch itemID {
	case DogMuttShopItemID:
		return DogMutt, 2_000, 0, true
	case DogShepherdShopItemID:
		return DogShepherd, 4_500, 10, true
	case DogMastiffShopItemID:
		return DogMastiff, 8_000, 20, true
	default:
		return DogNone, 0, 0, false
	}
}

// Sell 出售仓库果实换金币。不可卖种子/其他道具。
func (a *Aggregate) Sell(req SellReq) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if req.Quantity == 0 {
		return ActionResult{Err: pkgerr.BadQuantity}
	}
	crop, ok := gameconf.CropByID(req.ItemID)
	if !ok {
		return ActionResult{Err: pkgerr.ItemNotFound}
	}
	key := FruitItem(req.ItemID)
	have := a.Items[key]
	if have < req.Quantity {
		return ActionResult{Err: pkgerr.NotEnoughItem}
	}

	a.Items[key] -= req.Quantity
	if a.Items[key] == 0 {
		delete(a.Items, key)
	}
	a.Coin += int64(crop.FruitPrice) * int64(req.Quantity)
	a.FarmSeq++
	return a.okPatch(0)
}
