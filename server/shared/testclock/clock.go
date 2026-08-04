// Package testclock provides a process-local wall clock with a debug offset.
// Used by Farm (and Gateway fan-out) so smoke can advance growth without
// waiting on real demo-season sleep across process boundaries.
package testclock

import (
	"encoding/json"
	"io"
	"net/http"
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

// Advance adds ms to the debug offset. ms must be positive for HTTP handlers.
func (c *Clock) Advance(ms int64) {
	if c == nil || ms == 0 {
		return
	}
	c.offset.Add(ms)
}

type advanceRequest struct {
	MS int64 `json:"ms"`
}

// AdvanceHandler returns POST /internal/v1/debug/advance that advances this clock.
func (c *Clock) AdvanceHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req advanceRequest
		if err := json.Unmarshal(body, &req); err != nil || req.MS <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.Advance(req.MS)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]int64{"server_time": c.Now()})
	}
}
