package store

import "farm/server/gateway/presence"

// ConnectionRegistry returns the shared Redis-backed registry used to route
// Farm Delta callbacks to the Gateway holding each WebSocket.
func (s *Store) ConnectionRegistry() *presence.Registry {
	if s == nil {
		return presence.New(nil)
	}
	return presence.New(s.rdb)
}
