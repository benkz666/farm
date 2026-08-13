package store

import "farm/server/shared/presence"

// ConnectionRegistry returns the shared Redis-backed registry used to route
// Farm Delta callbacks to the Gateway holding each WebSocket.
func (s *Store) ConnectionRegistry() *presence.Registry {
	if s == nil {
		return presence.New(nil)
	}
	return presence.New(s.rdb)
}

// GatewayDirectory returns the Redis-backed ephemeral Gateway discovery
// directory used by Farm-to-Gateway push routing.
func (s *Store) GatewayDirectory() *presence.GatewayDirectory {
	if s == nil {
		return presence.NewGatewayDirectory(nil)
	}
	return presence.NewGatewayDirectory(s.rdb)
}
