# Task 6 Report — 双分片编排 + 4a smoke

状态：DONE

## 实现

- `deploy/compose.yml`：保留 MySQL/Redis/Kafka；新增 `profile=shards` 的 `farm-0/1` + `gateway-0/1`（镜像 `deploy/Dockerfile.server`）。
- `scripts/run.sh shards`：依赖 compose 起 MySQL/Redis/Kafka → 迁移 → 宿主机拉起四进程（9100/9101/9200/9201）+ Vite。
- `scripts/stop.sh`：清理双分片端口与 pid；`--compose` 带 `--profile shards down`。
- `server/cmd/smoke shards`：A 落 farm-0 连 gateway-0，B 落 farm-1 连 gateway-1，加好友后互相 EnterFarm，断言 `relation=FRIEND`。
- README / Makefile / `.env.example`：文档化双分片启动与 `make run-shards` / `make smoke-shards` / `make compose-shards`。

## 验证

```text
cd server && go test ./...
# 全部 ok

cd server && go vet ./... && go build ./cmd/smoke ./cmd/farm-server
# PASS

bash -n scripts/run.sh scripts/stop.sh
# SYNTAX_OK

docker compose -f deploy/compose.yml config --quiet
# PASS（仅校验编排文件）
```

本机 Docker daemon 未运行（无法连接 OrbStack socket），**未实测** `./scripts/run.sh shards` 与 `go run ./cmd/smoke shards` 双实例联调。编排文件与 smoke 代码已就绪，Docker 可用后按 README「期 4a」步骤验收。

## 边界

- 默认 `./scripts/run.sh` 仍为单进程 `FARM_ROLE=all`（:9002），不破坏既有开发路径。
- 本 Task 只覆盖跨片好友互相 EnterFarm；互助/偷菜 smoke 留给后续 Task。
