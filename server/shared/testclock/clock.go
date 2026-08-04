// Package testclock provides a process-local wall clock with a debug offset.
// Used by Farm (and Gateway fan-out) so smoke can advance growth without
// waiting on real demo-season sleep across process boundaries.
package testclock

import (
	"sync/atomic"
	"time"
)

// Clock is a Now() source with an additive millisecond offset.
type Clock struct {
	offset atomic.Int64
}

// Now returns Unix milliseconds including the debug offset.
func (c *Clock) Now() int64 {
	if c == nil {
		return time.Now().UnixMilli()
	}
	return time.Now().UnixMilli() + c.offset.Load()
}

// Advance adds ms to the debug offset.
func (c *Clock) Advance(ms int64) {
	if c == nil || ms == 0 {
		return
	}
	c.offset.Add(ms)
}
