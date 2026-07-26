// Package social 实现期 3 好友相关的纯函数：分享链接凭证的签发与校验。
//
// 凭证格式遵循 docs/design/protocol.md 3.3 节：
//
//	payload = base64url(JSON{inviter_uid, nonce, exp})
//	sig     = base64url(HMAC-SHA256(server_key, payload)[:16])
//	token   = payload + "." + sig
//
// 凭证无状态、不落库，过期由 exp（签发时刻 + 7 天）控制；吊销只能靠轮换 server_key。
package social

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"farm/server/internal/pkgerr"
)

// InviteTTL 是分享凭证的有效期，对应策划 18.7 与 protocol 3.3 的 7 天。
const InviteTTL int64 = 7 * 24 * 60 * 60 * 1000 // 毫秒

// sigSize 取 HMAC-SHA256 输出前 16 字节作为签名，与 protocol 3.3 一致。
const sigSize = 16

// invitePayload 是凭证 payload 部分的明文结构。
type invitePayload struct {
	InviterUID uint64 `json:"inviter_uid"`
	Nonce      string `json:"nonce"`
	Exp        int64  `json:"exp"`
}

// IssueInvite 用 server_key 为 inviterUID 签发一张 7 天有效的分享凭证。
// now 为毫秒墙钟时间。secret 不可为空，否则返回错误以防误用弱密钥。
func IssueInvite(inviterUID uint64, now int64, secret []byte) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("social: invite secret must not be empty")
	}

	nonce, err := randomNonce()
	if err != nil {
		return "", fmt.Errorf("social: generate invite nonce: %w", err)
	}

	p := invitePayload{
		InviterUID: inviterUID,
		Nonce:      nonce,
		Exp:        now + InviteTTL,
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("social: marshal invite payload: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(payload)
	sig := sign(encoded, secret)
	return encoded + "." + sig, nil
}

// ParseInvite 校验凭证并返回邀请人 uid。失败返回 protocol 4.5 的错误码：
//   - InviteInvalid(1406)：格式错误、签名不匹配或 payload 解析失败
//   - InviteExpired(1407)：签名通过但 now >= exp
//
// now 为毫秒墙钟时间。secret 必须与签发时一致。
func ParseInvite(token string, secret []byte, now int64) (uint64, pkgerr.Code) {
	dot := strings.LastIndex(token, ".")
	if dot <= 0 || dot == len(token)-1 {
		return 0, pkgerr.InviteInvalid
	}
	encoded := token[:dot]
	sig := token[dot+1:]

	wantSig := sign(encoded, secret)
	if !hmac.Equal([]byte(sig), []byte(wantSig)) {
		return 0, pkgerr.InviteInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, pkgerr.InviteInvalid
	}

	var p invitePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, pkgerr.InviteInvalid
	}
	if now > p.Exp {
		return 0, pkgerr.InviteExpired
	}
	return p.InviterUID, pkgerr.OK
}

// sign 用 secret 对 msg 计算 HMAC-SHA256 并取前 sigSize 字节，再做 base64url 编码。
func sign(msg string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(msg))
	sum := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:sigSize])
}

// randomNonce 生成 16 字节随机 nonce 并以 base64url 表示。
func randomNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}
