// Package errcode 定义客户端 Protobuf 契约使用的稳定业务错误码。
//
// 约定（见 protocol.md 4.1）：
//   - 0 表示成功
//   - 错误码分段，每段 100 个，段内留空位便于扩展
//   - 不允许返回未在 protocol.md 登记的错误码
package errcode

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

// 4.3 地块与种植（1200—1299）
const (
	PlotNotFound           Code = 1201 // ERR_PLOT_NOT_FOUND 地块不存在
	NotOwner               Code = 1202 // ERR_NOT_OWNER 只能在自己的农场进行此操作
	PlotNotWasteland       Code = 1203 // ERR_PLOT_NOT_WASTELAND 这块地已经翻过了
	PlotNotCleanable       Code = 1204 // ERR_PLOT_NOT_CLEANABLE 这块地没有需要清理的东西
	PlotNotTilled          Code = 1205 // ERR_PLOT_NOT_TILLED 请先锄地
	PlotNotGrowing         Code = 1206 // ERR_PLOT_NOT_GROWING 作物不在生长中
	PlotNotMature          Code = 1207 // ERR_PLOT_NOT_MATURE 作物还没成熟
	PlotEmpty              Code = 1208 // ERR_PLOT_EMPTY 这块地没有作物
	SeedNotOwned           Code = 1209 // ERR_SEED_NOT_OWNED 背包里没有这种种子
	CropLocked             Code = 1210 // ERR_CROP_LOCKED 等级不足，尚未解锁该作物
	AlreadyWatered         Code = 1211 // ERR_ALREADY_WATERED 水分充足，不需要浇水
	NoWeed                 Code = 1212 // ERR_NO_WEED 这块地没有杂草
	NoPest                 Code = 1213 // ERR_NO_PEST 这块地没有害虫
	FertilizerNotOwned     Code = 1214 // ERR_FERTILIZER_NOT_OWNED 背包里没有这种化肥
	StageAlreadyFertilized Code = 1215 // ERR_STAGE_ALREADY_FERTILIZED 当前生长阶段已经施过肥了
	HarvestedByOwner       Code = 1216 // ERR_HARVESTED_BY_OWNER 作物已被主人收获
	PlotWithered           Code = 1217 // ERR_PLOT_WITHERED 作物已经枯萎了
)

// 4.4 扩地与经济（1300—1399）。
const (
	LevelTooLow     Code = 1301 // ERR_LEVEL_TOO_LOW 等级不足
	NotEnoughCoin   Code = 1302 // ERR_NOT_ENOUGH_COIN 金币不足
	PlotLimit       Code = 1303 // ERR_PLOT_LIMIT 已达到地块上限
	ItemNotFound    Code = 1304 // ERR_ITEM_NOT_FOUND 商品不存在
	NotEnoughItem   Code = 1305 // ERR_NOT_ENOUGH_ITEM 数量不足
	ItemNotSellable Code = 1306 // ERR_ITEM_NOT_SELLABLE 该物品不可出售
	BadQuantity     Code = 1307 // ERR_BAD_QUANTITY 数量不合法
)

// 4.5 社交与偷菜（1400—1499）。
const (
	NotFriend             Code = 1401 // ERR_NOT_FRIEND 你们还不是好友
	AlreadyFriend         Code = 1402 // ERR_ALREADY_FRIEND 你们已经是好友了
	CannotFriendSelf      Code = 1403 // ERR_CANNOT_FRIEND_SELF 不能添加自己为好友
	FriendLimitSelf       Code = 1404 // ERR_FRIEND_LIMIT_SELF 你的好友数已达 200 上限
	FriendLimitPeer       Code = 1405 // ERR_FRIEND_LIMIT_PEER 对方好友数已达上限
	InviteInvalid         Code = 1406 // ERR_INVITE_INVALID 邀请链接无效
	InviteExpired         Code = 1407 // ERR_INVITE_EXPIRED 邀请链接已过期
	StealSelf             Code = 1408 // ERR_STEAL_SELF 不能偷自己的菜
	StealAlreadyDone      Code = 1409 // ERR_STEAL_ALREADY_DONE 这块地你本轮已经偷过了
	StealQuotaExhausted   Code = 1410 // ERR_STEAL_QUOTA_EXHAUSTED 这块地能偷的已经被偷光了
	StealIntercepted      Code = 1411 // ERR_STEAL_INTERCEPTED 被看家狗抓住了
	StealNoAfford         Code = 1412 // ERR_STEAL_NO_AFFORD 金币不足以承担被抓的赔付风险
	UserNotFound          Code = 1413 // ERR_USER_NOT_FOUND 用户不存在
	FriendRequestPending  Code = 1414 // ERR_FRIEND_REQUEST_PENDING 已发送过好友申请
	FriendRequestNotFound Code = 1415 // ERR_FRIEND_REQUEST_NOT_FOUND 申请不存在或已处理
)

// 4.6 宠物（1500—1599）。
const (
	DogAlreadyOwned Code = 1501 // ERR_DOG_ALREADY_OWNED 已经拥有这种狗了
	DogNotOwned     Code = 1502 // ERR_DOG_NOT_OWNED 还没有这种狗
	BowlFull        Code = 1503 // ERR_BOWL_FULL 狗盆已经满了
	NoDogFood       Code = 1504 // ERR_NO_DOG_FOOD 狗粮不足
	DogLocked       Code = 1505 // ERR_DOG_LOCKED 等级不足，尚未解锁这种狗
)

// 4.7 任务、邮件与图鉴（1600—1699）。
const (
	TaskNotComplete    Code = 1601 // ERR_TASK_NOT_COMPLETE 任务尚未完成
	TaskAlreadyClaimed Code = 1602 // ERR_TASK_ALREADY_CLAIMED 任务奖励已领取
	MailNotFound       Code = 1603 // ERR_MAIL_NOT_FOUND 邮件不存在或已过期
	MailNoAttachment   Code = 1604 // ERR_MAIL_NO_ATTACHMENT 这封邮件没有附件
	MailAlreadyClaimed Code = 1605 // ERR_MAIL_ALREADY_CLAIMED 附件已领取
)
