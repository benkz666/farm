COMPOSE_FILE := deploy/compose.yml

.PHONY: compose-up compose-down compose-shards migrate run run-gateway run-farm client-dev test smoke smoke-friends smoke-room smoke-shards smoke-all run-all run-shards stop-all

compose-up:
	docker compose -f $(COMPOSE_FILE) up -d

# 仅依赖（MySQL/Redis/Kafka）。双分片 farm/gateway 见 compose-shards 或 ./scripts/run.sh shards。
compose-down:
	docker compose -f $(COMPOSE_FILE) --profile shards down

compose-shards:
	docker compose -f $(COMPOSE_FILE) --profile shards up -d --build

migrate:
	docker compose -f $(COMPOSE_FILE) exec -T mysql mysql -ufarm -pfarm farm < server/migrations/001_init.sql
	docker compose -f $(COMPOSE_FILE) exec -T mysql mysql -ufarm -pfarm farm < server/migrations/002_items.sql
	docker compose -f $(COMPOSE_FILE) exec -T mysql mysql -ufarm -pfarm farm < server/migrations/003_friendship.sql

# 本地联调默认打开 debug 调时，供 make smoke 的 /api/debug/advance 使用；生产切勿导出。
run:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	export FARM_ALLOW_DEBUG_TIME=$${FARM_ALLOW_DEBUG_TIME:-1}; \
	cd server && go run ./cmd/farm-server

# 独立 Gateway：客户端 HTTP/WS 入口，EnterFarm/Till 通过内部 HTTP 转到目标 Farm。
run-gateway:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	export FARM_ROLE=gateway; \
	cd server && go run ./cmd/farm-server

# 独立 Farm：只提供 /internal/v1/cmd；FARM_INSTANCE_ID 必须在路由表中存在。
run-farm:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	export FARM_ROLE=farm; \
	cd server && go run ./cmd/farm-server

client-dev:
	cd client && npm run dev

test:
	cd server && go test ./...

# 需已 make run（或等价进程）且开启 FARM_ALLOW_DEBUG_TIME=1。
smoke:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/smoke

# 期 3 加好友冒烟：注册 A/B → GenShareLink → AcceptInvite → FriendList → 重复 Add 得 1402。
smoke-friends:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/smoke friends

# 期 3 房间同步冒烟：A、B 好友 → B Enter A → A Till → B 收 FarmDelta(9000) 与 SyncFarm 状态一致。
smoke-room:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/smoke room

# 种植 + 加好友 + 房间同步全链路冒烟。
smoke-all:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/smoke all

# 期 4a 双分片冒烟：A/B 落不同 Farm，分别连 gateway-0/1，互为好友后互相 EnterFarm。
# 需先 ./scripts/run.sh shards（或 make compose-shards 且已 migrate）。
smoke-shards:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/smoke shards

run-all:
	./scripts/run.sh

run-shards:
	./scripts/run.sh shards

stop-all:
	./scripts/stop.sh
