# Task 8 Report — 主人裁决互助 + Gateway 接线

状态：DONE

## 实现

- 新增 `cross.Owner`：订阅 `CrossAction`，在主人 Actor 内校验好友关系、执行浇水/除草/除虫、缓存最近 64 条裁决结果，并回发 `CrossResult`；提交后的 `FarmDelta` 保持广播。
- Gateway 在拜访好友农场时将 Water / RemoveWeed / RemovePest 转成异步 CrossAction；收到 CrossResult 后结算访客预占、发放奖励或回滚，并以原 `cmd/client_seq` 回应 WebSocket。
- 新增维护日计数（150 次上限）：成功互助按预占发放 +2 经验；除草/除虫额外 +5 金币；失败或 5 秒超时释放预占。计数写入既有 `player.daily_blob`，避免重连绕过限制。
- 运行时装配：单进程 `all` 使用内存 EventBus；分片 `gateway/farm` 使用 Kafka。Farm 角色订阅主人裁决，Gateway 角色订阅访客结算。

## 验证

```text
cd server && go test ./... && go vet ./... && go build ./cmd/farm-server
# PASS
```

覆盖场景：

- 非好友由主人侧拒绝；
- 已浇水返回 `ERR_ALREADY_WATERED`；
- 内存 EventBus 往返：主人提交、Delta 广播、访客获得经验并确认维护计数；
- 重复 `req_id` 仅执行一次主人写入。
