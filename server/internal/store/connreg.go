package store

import "farm/server/internal/connreg"

// ConnectionRegistry returns the shared Redis-backed registry used to route
// Farm Delta callbacks to the Gateway holding each WebSocket.
func (s *Store) ConnectionRegistry() *connreg.Registry {
	if s == nil {
		return connreg.New(nil)
	}
	return connreg.New(s.rdb)
}
