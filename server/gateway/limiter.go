package gateway

import "time"

const (
	connectionBurst               = 20
	connectionRate                = 10
	consecutiveRateLimitThreshold = 5
)

// connectionLimiter enforces the protocol's per-connection token bucket.
type connectionLimiter struct {
	tokens                float64
	last                  time.Time
	consecutiveRejections int
}

func newConnectionLimiter() *connectionLimiter {
	return &connectionLimiter{
		tokens: connectionBurst,
		last:   time.Now(),
	}
}

func (l *connectionLimiter) Allow() bool {
	// Only the connection's WebSocket read goroutine calls the limiter. Keeping
	// a mutex here added two atomic operations to every command without adding
	// safety; asynchronous writers never inspect this state.
	now := time.Now()
	l.tokens = min(float64(connectionBurst), l.tokens+now.Sub(l.last).Seconds()*connectionRate)
	l.last = now
	if l.tokens < 1 {
		l.consecutiveRejections++
		return false
	}
	l.tokens--
	l.consecutiveRejections = 0
	return true
}

// ShouldDisconnect reports whether a client exceeded the allowed consecutive
// rate-limit violations and should be considered an automated caller.
func (l *connectionLimiter) ShouldDisconnect() bool {
	return l.consecutiveRejections >= consecutiveRateLimitThreshold
}
