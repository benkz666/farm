// Package pkgerr 定义与 docs/design/protocol.md 第 4 章一致的协议错误码。
//
// 约定（见 protocol.md 4.1）：
//   - 0 表示成功
//   - 错误码分段，每段 100 个，段内留空位便于扩展
//   - 不允许返回未在 protocol.md 登记的错误码
package pkgerr

import "strconv"

// Code 为协议错误码类型，与 protocol.md 中的数字码一一对应。
type Code int

// Error 让协议错误码可直接作为服务层 error 返回，并可由 errors.Is 判定。
func (c Code) Error() string {
	return strconv.Itoa(int(c))
}

const (
	// OK 表示成功。
	OK Code = 0
)

// 4.2 通用与会话（1000—1199）
const (
	Internal      Code = 1001 // ERR_INTERNAL 服务繁忙，请稍后重试
	BadRequest    Code = 1002 // ERR_BAD_REQUEST 请求参数有误
	RateLimited   Code = 1003 // ERR_RATE_LIMITED 操作太快了，慢一点
	Timeout       Code = 1004 // ERR_TIMEOUT 操作超时，请重试
	DuplicateOK   Code = 1005 // ERR_DUPLICATE_OK 该操作已生效
	Redirect      Code = 1006 // ERR_REDIRECT 服务器切换中，正在重连
	ConfigStale   Code = 1007 // ERR_CONFIG_STALE 版本已更新，请刷新页面
	Unauthorized  Code = 1101 // ERR_UNAUTHORIZED 请先登录
	TokenExpired  Code = 1102 // ERR_TOKEN_EXPIRED 登录已过期，请重新登录
	UsernameTaken Code = 1103 // ERR_USERNAME_TAKEN 该用户名已被注册
	BadCredential Code = 1104 // ERR_BAD_CREDENTIAL 用户名或密码错误
	Kicked        Code = 1105 // ERR_KICKED 账号已在其他地方登录
)

// 4.5 社交与偷菜（1400—1499）——期 1 仅用到 NotFriend。
const (
	NotFriend Code = 1401 // ERR_NOT_FRIEND 你们还不是好友
)
