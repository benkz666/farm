package farm

import (
	"math"

	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

const (
	// DogFoodShopItemID is sold by the gram; its item count is the gram count.
	DogFoodShopItemID     uint16 = 2_000
	DogMuttShopItemID     uint16 = 2_001
	DogShepherdShopItemID uint16 = 2_002
	DogMastiffShopItemID  uint16 = 2_003

	DogBowlCapacityGrams uint32 = 120
	MuttMsPerGram        int64  = 1_500 // demo 档：土狗每缩放小时消耗 4g。
	ShepherdMsPerGram    int64  = 1_200 // demo 档：牧羊犬每缩放小时消耗 5g。
	MastiffMsPerGram     int64  = 857   // demo 档：藏獒每缩放小时消耗 7g。

	DogMaxLevel           uint8  = 5
	DogInterceptsPerLevel uint16 = 20
)

// DogType is stable in storage and protocol responses. Zero means no active dog.
type DogType uint8

const (
	DogNone DogType = iota
	DogMutt
	DogShepherd
	DogMastiff
	DogTypeCount
)

// PetState persists dog ownership and the bowl's self-describing expiration.
// BowlEmptyAt and MsPerGram retain the rate at the time food was added.
type PetState struct {
	ActiveDog   DogType   `json:"active_dog"`
	Owned       uint8     `json:"owned"`
	DogLevel    [3]uint8  `json:"dog_level"`
	Intercepts  [3]uint16 `json:"intercepts"`
	BowlEmptyAt int64     `json:"bowl_empty_at"`
	MsPerGram   int64     `json:"ms_per_gram"`
}

// PetDogStatus exposes one owned dog's independent growth state.
type PetDogStatus struct {
	DogType         DogType `json:"dog_type"`
	Level           uint8   `json:"level"`
	Intercepts      uint16  `json:"intercepts"`
	InterceptionPct uint8   `json:"interception_pct"`
}

// PetStatus is the client-facing projection of PetState.
type PetStatus struct {
	ActiveDog       DogType        `json:"active_dog"`
	Owned           uint8          `json:"owned"`
	BowlGrams       uint32         `json:"bowl_grams"`
	BowlEmptyAt     int64          `json:"bowl_empty_at"`
	MsPerGram       int64          `json:"ms_per_gram"`
	DogLevel        uint8          `json:"dog_level"`
	Intercepts      uint16         `json:"intercepts"`
	InterceptionPct uint8          `json:"interception_pct"`
	Dogs            []PetDogStatus `json:"dogs"`
}

// PetFeedReq feeds a positive number of dog-food grams.
type PetFeedReq struct {
	Grams uint32
}

// HasDog reports whether this farm owns a dog type.
func (p PetState) HasDog(dog DogType) bool {
	index, ok := dogIndex(dog)
	if !ok {
		return false
	}
	return p.Owned&(1<<uint(index)) != 0
}

// IsGuarding reports whether an active dog still has food.
func (p PetState) IsGuarding(now int64) bool {
	return p.HasDog(p.ActiveDog) && p.BowlEmptyAt > now && p.MsPerGram > 0
}

// ShouldIntercept applies the configured percentage to a uniformly distributed
// roll in [0, 99]. An empty bowl always yields false.
func (p PetState) ShouldIntercept(now int64, roll uint8) bool {
	return p.IsGuarding(now) && roll < p.interceptionPct()
}

func (p PetState) interceptionPct() uint8 {
	return p.interceptionPctFor(p.ActiveDog)
}

func (p PetState) interceptionPctFor(dog DogType) uint8 {
	index, ok := dogIndex(dog)
	if !ok {
		return 0
	}
	return dogBaseInterception(dog) + min(p.DogLevel[index], DogMaxLevel)
}

func (p *PetState) recordIntercept() {
	if p == nil {
		return
	}
	index, ok := dogIndex(p.ActiveDog)
	if !ok || !p.HasDog(p.ActiveDog) {
		return
	}
	if p.Intercepts[index] < math.MaxUint16 {
		p.Intercepts[index]++
	}
	level := uint8(p.Intercepts[index] / DogInterceptsPerLevel)
	if level > DogMaxLevel {
		level = DogMaxLevel
	}
	if level > p.DogLevel[index] {
		p.DogLevel[index] = level
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
	status := PetStatus{
		ActiveDog:   p.ActiveDog,
		Owned:       p.Owned,
		BowlGrams:   p.remainingGrams(now),
		BowlEmptyAt: p.BowlEmptyAt,
		MsPerGram:   p.MsPerGram,
		Dogs:        make([]PetDogStatus, 0, int(DogTypeCount-1)),
	}
	for dog := DogMutt; dog < DogTypeCount; dog++ {
		if !p.HasDog(dog) {
			continue
		}
		index, _ := dogIndex(dog)
		entry := PetDogStatus{
			DogType:         dog,
			Level:           min(p.DogLevel[index], DogMaxLevel),
			Intercepts:      p.Intercepts[index],
			InterceptionPct: p.interceptionPctFor(dog),
		}
		status.Dogs = append(status.Dogs, entry)
		if dog == p.ActiveDog {
			status.DogLevel = entry.Level
			status.Intercepts = entry.Intercepts
			status.InterceptionPct = entry.InterceptionPct
		}
	}
	if !p.HasDog(status.ActiveDog) {
		status.ActiveDog = DogNone
		status.BowlGrams = 0
		status.BowlEmptyAt = 0
		status.MsPerGram = 0
	}
	return status
}

// PetStatus returns the current pet view at now.
func (a *Aggregate) PetStatus(now int64) PetStatus {
	if a == nil {
		return PetStatus{}
	}
	return a.Pet.Status(now)
}

// PetActivate changes the only active guard dog and preserves the remaining
// whole grams while applying the newly active breed's consumption rate.
func (a *Aggregate) PetActivate(dog DogType, now int64) ActionResult {
	return a.PetActivateWithProfile(dog, now, gameconf.TimeProfileDemo)
}

// PetActivateWithProfile switches breed using the server-authoritative rate.
// Existing callers retain demo semantics through PetActivate.
func (a *Aggregate) PetActivateWithProfile(dog DogType, now int64, timeProfile string) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if !a.Pet.HasDog(dog) {
		return ActionResult{Err: pkgerr.DogNotOwned}
	}
	if a.Pet.ActiveDog == dog {
		return a.okPatch(0)
	}
	remaining := a.Pet.remainingGrams(now)
	a.Pet.ActiveDog = dog
	a.Pet.MsPerGram = dogMsPerGram(dog, timeProfile)
	if remaining > 0 {
		a.Pet.BowlEmptyAt = now + int64(remaining)*a.Pet.MsPerGram
	} else {
		a.Pet.BowlEmptyAt = 0
	}
	a.FarmSeq++
	return a.okPatch(0)
}

// PetFeed tops up the bowl without exceeding capacity. Excess requested grams
// remain in the bag, so a retry cannot consume food that did not fit.
func (a *Aggregate) PetFeed(req PetFeedReq, now int64) ActionResult {
	return a.PetFeedWithProfile(req, now, gameconf.TimeProfileDemo)
}

// PetFeedWithProfile applies the current server profile to newly refilled food.
// BowlEmptyAt remains self-describing between later profile changes.
func (a *Aggregate) PetFeedWithProfile(req PetFeedReq, now int64, timeProfile string) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if req.Grams == 0 {
		return ActionResult{Err: pkgerr.BadQuantity}
	}
	rate := dogMsPerGram(a.Pet.ActiveDog, timeProfile)
	if rate <= 0 || !a.Pet.HasDog(a.Pet.ActiveDog) {
		return ActionResult{Err: pkgerr.DogNotOwned}
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
	a.Pet.MsPerGram = rate
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

func dogIndex(dog DogType) (int, bool) {
	if dog < DogMutt || dog >= DogTypeCount {
		return 0, false
	}
	return int(dog - DogMutt), true
}

func dogBaseInterception(dog DogType) uint8 {
	switch dog {
	case DogMutt:
		return 25
	case DogShepherd:
		return 35
	case DogMastiff:
		return 45
	default:
		return 0
	}
}

func dogMsPerGram(dog DogType, timeProfile string) int64 {
	hourMs := gameconf.HourMs(timeProfile)
	if hourMs <= 0 {
		hourMs = gameconf.HourMs(gameconf.TimeProfileDemo)
	}
	switch dog {
	case DogMutt:
		return hourMs / 4
	case DogShepherd:
		return hourMs / 5
	case DogMastiff:
		return hourMs / 7
	default:
		return 0
	}
}
