// Package pkgjson 提供跨语言 JSON 约定类型。
package pkgjson

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// UID 在 JSON 中编解码为十进制字符串，避免 JS Number 精度丢失（雪花 > 2^53-1）。
// 入站同时接受 JSON number（兼容旧客户端）与 string。
type UID uint64

// MarshalJSON 始终输出带引号的十进制字符串。
func (u UID) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatUint(uint64(u), 10) + `"`), nil
}

// UnmarshalJSON 接受 "123" 或 123。
func (u *UID) UnmarshalJSON(b []byte) error {
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
			return fmt.Errorf("pkgjson: uid %q: %w", s, err)
		}
		*u = UID(v)
		return nil
	}
	var v uint64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*u = UID(v)
	return nil
}
