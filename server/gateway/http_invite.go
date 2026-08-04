package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"farm/server/shared/errcode"
	socialapi "farm/server/socialsvr/api"
)

type inviteLandingResponse struct {
	OK  bool         `json:"ok"`
	Err errcode.Code `json:"err"`
}

// inviteLanding 实现 GET /i/<token>（规格 phase3 §4.2）：
//   - 无会话或会话失效：HTTP 302 重定向到前端登录页并带上 invite 参数
//     `/login?invite=<token>`，由 Task 9 的登录页在登录后继续 AcceptInvite
//   - 请求携带 `Authorization: Bearer <session_token>` 且会话有效：服务端
//     直接 AcceptInvite（ParseInvite + AddFriends），返回 JSON
//     `{ "ok": true, "err": 0 }` 或错误码 JSON
//
// 不带 Bearer 时永远不返回 need_login=true，避免客户端无登录引导可循。
func (g *Gateway) inviteLanding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, errcode.BadRequest, http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/i/")
	if token == "" || strings.Contains(token, "/") {
		writeHTTPError(w, errcode.BadRequest, http.StatusBadRequest)
		return
	}

	uid, ok := g.sessionUIDFromRequest(r)
	if !ok {
		// 未登录：引导到前端登录页并带上 invite 参数，登录后继续 AcceptInvite。
		target := "/login?invite=" + url.QueryEscape(token)
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	errCode := g.acceptInviteForUser(r.Context(), uid, token)
	writeJSON(w, http.StatusOK, inviteLandingResponse{OK: errCode == errcode.OK, Err: errCode})
}

// sessionUIDFromRequest 从 `Authorization: Bearer <session_token>` 解析会话 uid。
// 返回 (uid, true) 表示有效会话；(0, false) 表示无凭据或失效，需要登录引导。
func (g *Gateway) sessionUIDFromRequest(r *http.Request) (uint64, bool) {
	if g.sessions == nil {
		return 0, false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return 0, false
	}
	sessionToken := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	if sessionToken == "" {
		return 0, false
	}
	uid, err := g.sessions.Get(r.Context(), sessionToken)
	if err != nil || uid == 0 {
		return 0, false
	}
	return uid, true
}

// acceptInviteForUser 复用 ws_friends 的 ParseInvite + AddFriends 逻辑，
// 供 HTTP 落地页在已登录会话下直接建立好友关系。
func (g *Gateway) acceptInviteForUser(ctx context.Context, uid uint64, token string) errcode.Code {
	if g.friends == nil {
		return errcode.Internal
	}
	if len(g.inviteSecret) == 0 {
		return errcode.Internal
	}
	inviterUID, code := socialapi.ParseInvite(token, g.inviteSecret, g.Now())
	if code != errcode.OK {
		return code
	}
	if inviterUID == 0 {
		return errcode.InviteInvalid
	}
	return g.addFriends(uid, inviterUID)
}
