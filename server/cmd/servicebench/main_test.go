package main

import (
	"testing"
	"time"
)

func TestMarkOutstandingFailed(t *testing.T) {
	recorded := &recorder{}
	recorded.succeeded.Store(7)
	recorded.failed.Store(2)

	markOutstandingFailed(recorded, 12)
	markOutstandingFailed(recorded, 12)

	if got := recorded.failed.Load(); got != 5 {
		t.Fatalf("failed = %d, want 5", got)
	}
}

func TestSummarizeTimedOut(t *testing.T) {
	recorded := &recorder{}
	recorded.add(2*time.Millisecond, true, 0)
	measured := summarize("test", 10, 1, time.Second, recorded, true)

	if !measured.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}
	if measured.ActualQPS != 1 {
		t.Fatalf("ActualQPS = %v, want 1", measured.ActualQPS)
	}
}
