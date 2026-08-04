// Package clientjson 提供跨语言 JSON 约定类型。
package clientjson

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Uint64 在 JSON 中编解码为十进制字符串，避免 JS Number 的 2^53-1 精度上限。
// 入站同时接受 JSON number（兼容旧客户端）与 string。
type Uint64 uint64

// Int64 在 JSON 中同样编码为十进制字符串。它用于金币、奖励等允许服务端
// 使用 int64 保存、但不能让浏览器先解析成 Number 的业务数值。
// 入站同时接受旧版 JSON number 与字符串。
type Int64 int64

// UID 是 Uint64 的语义别名；所有 uid 都沿用同一套跨语言编码规则。
type UID = Uint64

// MarshalJSON 始终输出带引号的十进制字符串。
func (u Uint64) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatUint(uint64(u), 10) + `"`), nil
}

// UnmarshalJSON 接受 "123" 或 123。
func (u *Uint64) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*u = 0
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*u = 0
			return nil
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("pkgjson: uint64 %q: %w", s, err)
		}
		*u = Uint64(v)
		return nil
	}
	var v uint64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*u = Uint64(v)
	return nil
}

// MarshalJSON 始终输出带引号的十进制字符串。
func (i Int64) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(i), 10) + `"`), nil
}

// UnmarshalJSON 接受 "-123"、"123" 或旧版 JSON number。
func (i *Int64) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*i = 0
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*i = 0
			return nil
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("pkgjson: int64 %q: %w", s, err)
		}
		*i = Int64(v)
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*i = Int64(v)
	return nil
}
