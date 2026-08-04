package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const tokenEntropySize = 32

var tokenHMACKey = newTokenHMACKey()

func issueToken() (string, error) {
	var nonce [tokenEntropySize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("auth: generate token nonce: %w", err)
	}

	mac := hmac.New(sha256.New, tokenHMACKey[:])
	_, _ = mac.Write(nonce[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func newTokenHMACKey() [tokenEntropySize]byte {
	var key [tokenEntropySize]byte
	if _, err := rand.Read(key[:]); err != nil {
		// 无法获得安全随机源时，启动成功会令会话 token 可预测，因此必须拒绝启动。
		panic(fmt.Sprintf("auth: generate token HMAC key: %v", err))
	}
	return key
}
