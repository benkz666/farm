// Package api owns signed friend-invite tokens. The signed body is Protobuf;
// service and token contracts therefore share the same generated schema.
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"

	"google.golang.org/protobuf/proto"
)

// InviteTTL 是分享凭证的有效期，对应策划 18.7 与 protocol 3.3 的 7 天。
const InviteTTL int64 = 7 * 24 * 60 * 60 * 1000 // 毫秒

// sigSize 取 HMAC-SHA256 输出前 16 字节作为签名，与 protocol 3.3 一致。
const sigSize = 16

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

	p := &farmv1.InviteTokenPayload{
		InviterUid:  inviterUID,
		Nonce:       nonce,
		ExpiresAtMs: now + InviteTTL,
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
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
func ParseInvite(token string, secret []byte, now int64) (uint64, errcode.Code) {
	dot := strings.LastIndex(token, ".")
	if dot <= 0 || dot == len(token)-1 {
		return 0, errcode.InviteInvalid
	}
	encoded := token[:dot]
	sig := token[dot+1:]

	wantSig := sign(encoded, secret)
	if !hmac.Equal([]byte(sig), []byte(wantSig)) {
		return 0, errcode.InviteInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, errcode.InviteInvalid
	}

	p := &farmv1.InviteTokenPayload{}
	if err := proto.Unmarshal(payload, p); err != nil || p.InviterUid == 0 || len(p.Nonce) != 16 || p.ExpiresAtMs <= 0 {
		return 0, errcode.InviteInvalid
	}
	if now > p.ExpiresAtMs {
		return 0, errcode.InviteExpired
	}
	return p.InviterUid, errcode.OK
}

// sign 用 secret 对 msg 计算 HMAC-SHA256 并取前 sigSize 字节，再做 base64url 编码。
func sign(msg string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(msg))
	sum := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:sigSize])
}

// randomNonce 生成 16 字节随机 nonce 并以 base64url 表示。
func randomNonce() ([]byte, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:]...), nil
}
