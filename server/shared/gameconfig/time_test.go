package gameconfig

import (
	"sync"
	"testing"
	"time"
)

func TestTimeProfileSwitchHotUpdatesSafely(t *testing.T) {
	profiles := NewTimeProfileSwitch(TimeProfileFast)
	if got := profiles.Get(); got != TimeProfileFast {
		t.Fatalf("initial profile = %q, want fast", got)
	}
	if profiles.Set("turbo") {
		t.Fatal("accepted unsupported profile")
	}
	if got := profiles.Get(); got != TimeProfileFast {
		t.Fatalf("invalid update changed profile to %q", got)
	}

	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			profile := []string{TimeProfileDemo, TimeProfileFast, TimeProfileAuthentic}[index%3]
			for j := 0; j < 100; j++ {
				profiles.Set(profile)
				if !ValidTimeProfile(profiles.Get()) {
					t.Errorf("concurrent read returned invalid profile")
					return
				}
			}
		}(i)
	}
	wait.Wait()
}

func TestLocalDayKeyAndNextResetAtMidnightBoundaries(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.FixedZone("UTC+8", 8*60*60)
	t.Cleanup(func() { time.Local = originalLocal })

	beforeMidnight := time.Date(2026, time.July, 30, 23, 59, 59, 999*int(time.Millisecond), time.Local).UnixMilli()
	midnight := time.Date(2026, time.July, 31, 0, 0, 0, 0, time.Local).UnixMilli()
	nextMidnight := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.Local).UnixMilli()

	if got := LocalDayKey(beforeMidnight); got != 20260730 {
		t.Fatalf("LocalDayKey(23:59:59.999) = %d, want 20260730", got)
	}
	if got := NextLocalDayResetMs(beforeMidnight); got != midnight {
		t.Fatalf("NextLocalDayResetMs(23:59:59.999) = %d, want %d", got, midnight)
	}
	if got := LocalDayKey(midnight); got != 20260731 {
		t.Fatalf("LocalDayKey(00:00:00) = %d, want 20260731", got)
	}
	if got := NextLocalDayResetMs(midnight); got != nextMidnight {
		t.Fatalf("NextLocalDayResetMs(00:00:00) = %d, want %d", got, nextMidnight)
	}
}
