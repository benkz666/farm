package gateway

import (
	"sync/atomic"
	"testing"
)

func TestAllocateConnIDSkipsZeroFromSeed(t *testing.T) {
	t.Parallel()

	var counter atomic.Uint64
	counter.Store(0)
	if got := allocateConnID(&counter); got != 1 {
		t.Fatalf("allocateConnID from seed 0 = %d, want 1", got)
	}
}

func TestAllocateConnIDSkipsZeroAfterOverflow(t *testing.T) {
	t.Parallel()

	var counter atomic.Uint64
	counter.Store(^uint64(0)) // MaxUint64; Add(1) wraps to 0 and must be skipped.
	got := allocateConnID(&counter)
	if got == 0 {
		t.Fatal("allocateConnID after overflow returned 0")
	}
	if got != 1 {
		t.Fatalf("allocateConnID after overflow = %d, want 1", got)
	}
}

func TestDifferentConnIDSeedsProduceDifferentFirstIDs(t *testing.T) {
	t.Parallel()

	firstA := firstConnIDAfterSeed(0x1111_2222_3333_4444)
	firstB := firstConnIDAfterSeed(0xaaaa_bbbb_cccc_dddd)
	if firstA == 0 || firstB == 0 {
		t.Fatalf("first IDs must be non-zero: a=%d b=%d", firstA, firstB)
	}
	if firstA == firstB {
		t.Fatalf("different seeds produced the same first conn ID %d", firstA)
	}
}

func firstConnIDAfterSeed(seed uint64) uint64 {
	var counter atomic.Uint64
	counter.Store(seed)
	return allocateConnID(&counter)
}
