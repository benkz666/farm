package telemetry

import (
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	defaultMu     sync.RWMutex
	defaultLogger = NewLogger(os.Stderr, slog.LevelInfo)
)

// NewLogger builds a JSON slog logger with stable field conventions.
// Callers must not log tokens, secrets, or full request payloads.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}

// SetDefault replaces the process-wide obs logger (typically at startup).
func SetDefault(l *slog.Logger) {
	if l == nil {
		return
	}
	defaultMu.Lock()
	defaultLogger = l
	defaultMu.Unlock()
}

// L returns the process-wide structured logger.
func L() *slog.Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultLogger
}
