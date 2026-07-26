COMPOSE_FILE := deploy/compose.yml

.PHONY: compose-up compose-down migrate run client-dev test smoke smoke-friends smoke-room smoke-all run-all stop-all

compose-up:
	docker compose -f $(COMPOSE_FILE) up -d

compose-down:
	docker compose -f $(COMPOSE_FILE) down

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

run-all:
	./scripts/run.sh

stop-all:
	./scripts/stop.sh
