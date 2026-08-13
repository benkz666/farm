package gateway

import (
	"net/http"
	"net/url"
	"strings"
)

// sameOrigin accepts browser WebSocket upgrades only from the HTTP endpoint
// that serves the Gateway. Non-browser clients may omit Origin.
func sameOrigin(request *http.Request) bool {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, request.Host)
}
