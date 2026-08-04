package crossfarm

import (
	"testing"
	"time"
)

func TestDispatcherRetryBackoffSaturatesWithoutOverflow(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: dispatcherMinBackoff},
		{attempts: 1, want: 2 * dispatcherMinBackoff},
		{attempts: 7, want: 3200 * time.Millisecond},
		{attempts: 8, want: dispatcherMaxBackoff},
		{attempts: 63, want: dispatcherMaxBackoff},
		{attempts: 1_000_000, want: dispatcherMaxBackoff},
	}
	for _, test := range tests {
		if got := dispatcherRetryBackoff(test.attempts); got != test.want {
			t.Fatalf("attempts=%d backoff=%v, want %v", test.attempts, got, test.want)
		}
	}
}
