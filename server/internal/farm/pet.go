package farm

import (
	"farm/server/internal/pkgerr"
)

const (
	// DogFoodShopItemID is sold by the gram; its item count is the gram count.
	DogFoodShopItemID uint16 = 2_000
	DogMuttShopItemID uint16 = 2_001

	DogBowlCapacityGrams uint32 = 120
	MuttMsPerGram        int64  = 1_500 // demo 档：土狗每缩放小时消耗 4g。
)

// DogType is stable in storage and protocol responses. Zero means no active dog.
type DogType uint8

const (
	DogNone DogType = iota
	DogMutt
)

// PetState persists dog ownership and the bowl's self-describing expiration.
// BowlEmptyAt and MsPerGram retain the rate at the time food was added.
type PetState struct {
	ActiveDog   DogType   `json:"active_dog"`
	Owned       uint8     `json:"owned"`
	DogLevel    [1]uint8  `json:"dog_level"`
	Intercepts  [1]uint16 `json:"intercepts"`
	BowlEmptyAt int64     `json:"bowl_empty_at"`
	MsPerGram   int64     `json:"ms_per_gram"`
}

// PetStatus is the client-facing projection of PetState.
type PetStatus struct {
	ActiveDog       DogType `json:"active_dog"`
	Owned           uint8   `json:"owned"`
	BowlGrams       uint32  `json:"bowl_grams"`
	BowlEmptyAt     int64   `json:"bowl_empty_at"`
	DogLevel        uint8   `json:"dog_level"`
	Intercepts      uint16  `json:"intercepts"`
	InterceptionPct uint8   `json:"interception_pct"`
}

// PetFeedReq feeds a positive number of dog-food grams.
type PetFeedReq struct {
	Grams uint32
}

// HasDog reports whether this farm owns a dog type.
func (p PetState) HasDog(dog DogType) bool {
	if dog == DogNone || dog > DogMutt {
		return false
	}
	return p.Owned&(1<<uint(dog-1)) != 0
}

// IsGuarding reports whether an active dog still has food.
func (p PetState) IsGuarding(now int64) bool {
	return p.ActiveDog != DogNone && p.BowlEmptyAt > now && p.MsPerGram > 0
}

// ShouldIntercept applies the configured percentage to a uniformly distributed
// roll in [0, 99]. An empty bowl always yields false.
func (p PetState) ShouldIntercept(now int64, roll uint8) bool {
	return p.IsGuarding(now) && roll < p.interceptionPct()
}

func (p PetState) interceptionPct() uint8 {
	if p.ActiveDog != DogMutt {
		return 0
	}
	return 25 + p.DogLevel[0]
}

func (p *PetState) recordIntercept() {
	if p == nil || p.ActiveDog != DogMutt {
		return
	}
	p.Intercepts[0]++
	if p.DogLevel[0] < 5 && p.Intercepts[0]/20 > uint16(p.DogLevel[0]) {
		p.DogLevel[0]++
	}
}

func (p PetState) remainingGrams(now int64) uint32 {
	if p.BowlEmptyAt <= now || p.MsPerGram <= 0 {
		return 0
	}
	return uint32((p.BowlEmptyAt - now) / p.MsPerGram)
}

// Status returns a derived view rather than persisting a mutable food counter.
func (p PetState) Status(now int64) PetStatus {
	return PetStatus{
		ActiveDog:       p.ActiveDog,
		Owned:           p.Owned,
		BowlGrams:       p.remainingGrams(now),
		BowlEmptyAt:     p.BowlEmptyAt,
		DogLevel:        p.DogLevel[0],
		Intercepts:      p.Intercepts[0],
		InterceptionPct: p.interceptionPct(),
	}
}

// PetStatus returns the current pet view at now.
func (a *Aggregate) PetStatus(now int64) PetStatus {
	if a == nil {
		return PetStatus{}
	}
	return a.Pet.Status(now)
}

// PetActivate changes the only active guard dog.
func (a *Aggregate) PetActivate(dog DogType) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if !a.Pet.HasDog(dog) {
		return ActionResult{Err: pkgerr.DogNotOwned}
	}
	a.Pet.ActiveDog = dog
	a.FarmSeq++
	return a.okPatch(0)
}

// PetFeed tops up the bowl without exceeding capacity. Excess requested grams
// remain in the bag, so a retry cannot consume food that did not fit.
func (a *Aggregate) PetFeed(req PetFeedReq, now int64) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if req.Grams == 0 {
		return ActionResult{Err: pkgerr.BadQuantity}
	}
	remaining := a.Pet.remainingGrams(now)
	if remaining >= DogBowlCapacityGrams {
		return ActionResult{Err: pkgerr.BowlFull}
	}
	add := req.Grams
	if capacity := DogBowlCapacityGrams - remaining; add > capacity {
		add = capacity
	}
	key := DogFoodItem()
	if a.Items[key] < add {
		return ActionResult{Err: pkgerr.NoDogFood}
	}
	a.Items[key] -= add
	if a.Items[key] == 0 {
		delete(a.Items, key)
	}
	a.Pet.MsPerGram = MuttMsPerGram
	a.Pet.BowlEmptyAt = now + int64(remaining+add)*a.Pet.MsPerGram
	a.FarmSeq++
	return a.okPatch(0)
}

// FreezeStealCompensation deducts the potential loss before publishing a cross
// steal action. Only the visitor Actor calls this, so the balance cannot be
// spent during the owner-side decision window.
func (a *Aggregate) FreezeStealCompensation(amount int64) pkgerr.Code {
	if a == nil || amount <= 0 {
		return pkgerr.BadRequest
	}
	if a.Coin < amount {
		return pkgerr.StealNoAfford
	}
	a.Coin -= amount
	a.FarmSeq++
	return pkgerr.OK
}

// ReleaseStealCompensation returns a non-forfeited freeze to the visitor.
func (a *Aggregate) ReleaseStealCompensation(amount int64) {
	if a == nil || amount <= 0 {
		return
	}
	a.Coin += amount
	a.FarmSeq++
}

// ReceiveStealCompensation credits a dog interception to the farm owner.
func (a *Aggregate) ReceiveStealCompensation(amount int64) {
	if a == nil || amount <= 0 {
		return
	}
	a.Coin += amount
}

// RecordPetIntercept records progress as part of an already committed steal
// action; that action owns the corresponding FarmSeq increment.
func (a *Aggregate) RecordPetIntercept() {
	if a == nil {
		return
	}
	a.Pet.recordIntercept()
}
