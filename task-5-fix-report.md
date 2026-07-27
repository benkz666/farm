# Task 5 返工报告 — 异步 Delta 扇出与发起连接排除

状态：DONE

## 修复

- Farm RPC 在权威写入完成后启动异步 Delta 扇出，不再等待跨 Gateway HTTP 回推完成才返回 `Till` 或 `EnterFarm` 的响应。
- `farmrpc.CommandRequest` 新增 `originator_conn_id`；Gateway 转发远程 `EnterFarm` 与 `Till` 时填入发起 WebSocket 连接 ID。
- `FanoutPublisher` 扇出时跳过 `originator_conn_id` 对应的订阅连接，与本地 `BroadcastExcept` 行为对齐；发起客户端仅收到命令响应中的权威 Patch，不再重复收到 `FarmDelta`。

## 回归测试

- 红测：慢速 Delta Publisher 会阻塞原有 `Till` 响应；修复后响应先返回，扇出仍会异步启动。
- `TestFanoutPublisherSkipsOriginatingConnection`：确认只向其他房间订阅者投递 Delta。
- `TestGatewayRoutesEnterFarmAndTillThroughFarmRPC`：确认 Gateway 为远程 Farm 命令传递发起连接 ID。

## 验证

```text
cd server && go test ./... -count=1
# PASS

cd server && go test -race ./internal/farmrpc -count=1
# PASS
```
