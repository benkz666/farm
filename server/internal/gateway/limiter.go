package gateway

import (
	"sync"
	"time"
)

const (
	connectionBurst = 20
	connectionRate  = 10
)

// connectionLimiter enforces the protocol's per-connection token bucket.
type connectionLimiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newConnectionLimiter() *connectionLimiter {
	return &connectionLimiter{
		tokens: connectionBurst,
		last:   time.Now(),
	}
}

func (l *connectionLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.tokens = min(float64(connectionBurst), l.tokens+now.Sub(l.last).Seconds()*connectionRate)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
