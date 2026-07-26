# Task 6：Actor 运行时报告

## 交付

- 新增 `server/internal/actor`，以每 uid 一个 goroutine、一个无缓冲 mailbox 的方式串行处理 `Runtime.Do` 回调。
- 首次消息通过 `FarmStore.LoadFarm` 加载聚合；同 uid 并发消息在 mailbox 排队，加载仅执行一次。
- Actor 默认空闲 10 分钟后执行 `SaveFarm` 并卸载；构造函数接受短 TTL 以支持测试。
- 空闲 flush 失败时保留内存聚合并重试，避免未持久化状态被卸载；加载失败会返回包装错误并卸载该 Actor，后续请求可重新加载。

## TDD 与 race 证据

1. 先创建并发串行与空闲卸载测试，尚未实现运行时时运行：

   ```text
   cd server && go test ./internal/actor
   undefined: NewRuntime
   undefined: FarmActor
   FAIL farm/server/internal/actor [build failed]
   ```

   失败原因是预期的 Actor API 尚未实现。

2. 实现最小 mailbox 运行时后：

   ```text
   cd server && go test ./internal/actor
   ok   farm/server/internal/actor 0.309s
   ```

3. 最终全量与竞态验证：

   ```text
   cd server && go test ./...
   ok   farm/server/internal/actor 0.291s
   ok   farm/server/internal/auth
   ok   farm/server/internal/pkgerr
   ok   farm/server/internal/store

   cd server && go test -race ./internal/actor
   ok   farm/server/internal/actor 1.587s
   ```

## 测试覆盖

- `TestRuntimeSerializesConcurrentCallsForSameUID`：阻塞首个回调，断言第二个同 uid 回调不能先执行；最终金币递增两次且只加载一次。
- `TestRuntimeFlushesAndUnloadsIdleActor`：短 TTL 后断言 `SaveFarm` 被调用，下一次 `Do` 重新加载。
- `TestRuntimeReturnsLoadErrorAndUnloadsActor`：加载失败不执行回调，下一次请求可创建新 Actor 并成功加载。

## 范围与注意事项

- 本任务不实现定时 write-behind、优雅下线或跨实例迁移，符合期 1 最小 Actor 范围。
- `Runtime.Do` 没有 context 参数，加载与 flush 使用 `context.Background()`；取消、shutdown drain 将在网关/进程生命周期接线时补充。

## 审查跟进：回调 panic 死锁

- 问题：`run` 中直接调用 `fn` 时若 panic，不会向 `req.result` 写回，调用方在 `<-req.result` 永久阻塞。
- 修复：`invokeCallback` 对回调做 `recover`，将 panic 包装为 `actor: callback panic: ...` 写回 result；随后卸载 Actor（不落盘），下次 `Do` 可重新加载。
- 单测：`TestRuntimeRecoversCallbackPanicAndUnblocksCaller`。
- 验证：`cd server && go test -race ./internal/actor/ -count=1` → `ok farm/server/internal/actor`。
