package gateway

import "time"

const admissionWait = 2 * time.Millisecond

// acquireBoundedSlot first uses the allocation-free fast path. Only a request
// arriving at a momentary saturation point waits briefly for an in-flight
// operation to finish; sustained overload is still rejected instead of being
// converted into an unbounded queue.
func acquireBoundedSlot(slots chan struct{}) bool {
	return acquireBoundedSlotWithin(slots, admissionWait)
}

func acquireBoundedSlotWithin(slots chan struct{}, wait time.Duration) bool {
	if slots == nil {
		return true
	}
	select {
	case slots <- struct{}{}:
		return true
	default:
	}

	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func releaseBoundedSlot(slots chan struct{}) {
	if slots == nil {
		return
	}
	select {
	case <-slots:
	default:
	}
}
