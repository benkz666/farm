# Task 5 Report — 连接注册表 + Delta 跨 Gateway 回推

状态：DONE

## 实现

- 新增 Redis 连接注册表：登记 WS 生命周期，并维护按农场主 UID 的房间订阅索引。
- Gateway 在握手、进入农场、离开/断开连接时同步注册表；新增受服务间 token 保护的 Delta 回推端点。
- Farm 的远程 `Till` 提交后查询订阅索引，通过 HTTP 回推目标 Gateway；回推失败不回滚已提交的农场操作，客户端可用 `SyncFarm` 补齐。
- 增加 `FARM_GATEWAY_URLS` 配置，Farm 角色启动时要求配置目标 Gateway。

## 验证

```text
cd server && go test ./...
ok   farm/server/cmd/farm-server
ok   farm/server/internal/connreg
ok   farm/server/internal/farmrpc
ok   farm/server/internal/gateway
...
```
