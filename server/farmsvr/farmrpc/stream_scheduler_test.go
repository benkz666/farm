package farmrpc

import "testing"

func TestStreamSchedulerSizingScalesWithProcessors(t *testing.T) {
	one := streamSchedulerSizingFor(1)
	two := streamSchedulerSizingFor(2)
	if two.normalConcurrency != one.normalConcurrency*2 {
		t.Fatalf("normal concurrency: one=%d two=%d", one.normalConcurrency, two.normalConcurrency)
	}
	if two.barrierConcurrency != one.barrierConcurrency*2 {
		t.Fatalf("barrier concurrency: one=%d two=%d", one.barrierConcurrency, two.barrierConcurrency)
	}
	if two.normalCapacity != one.normalCapacity*2 || two.barrierCapacity != one.barrierCapacity*2 {
		t.Fatalf("queue capacity does not scale: one=%+v two=%+v", one, two)
	}
}

func TestStreamSchedulerSizingUsesSafeMinimum(t *testing.T) {
	sizing := streamSchedulerSizingFor(0)
	if sizing.normalConcurrency < streamMinimumNormalConcurrency {
		t.Fatalf("normal concurrency=%d", sizing.normalConcurrency)
	}
	if sizing.barrierConcurrency < streamMinimumBarrierConcurrency {
		t.Fatalf("barrier concurrency=%d", sizing.barrierConcurrency)
	}
}
