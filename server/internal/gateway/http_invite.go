package gateway

import (
	"net/http"
	"strings"

	"farm/server/internal/pkgerr"
)

type inviteLandingResponse struct {
	OK        bool   `json:"ok"`
	NeedLogin bool   `json:"need_login"`
	Token     string `json:"token"`
}

// inviteLanding returns the token for the client login flow to resume with
// AcceptInvite after the user establishes a WebSocket session.
func (g *Gateway) inviteLanding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/i/")
	if token == "" || strings.Contains(token, "/") {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, inviteLandingResponse{
		OK:        true,
		NeedLogin: true,
		Token:     token,
	})
}
