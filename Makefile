COMPOSE_FILE := deploy/compose.yml

.PHONY: compose-up compose-down migrate run client-dev test smoke run-all stop-all

compose-up:
	docker compose -f $(COMPOSE_FILE) up -d

compose-down:
	docker compose -f $(COMPOSE_FILE) down

migrate:
	docker compose -f $(COMPOSE_FILE) exec -T mysql mysql -ufarm -pfarm farm < server/migrations/001_init.sql
	docker compose -f $(COMPOSE_FILE) exec -T mysql mysql -ufarm -pfarm farm < server/migrations/002_items.sql

# FARM_ALLOW_DEBUG_TIME=1：开放 /api/debug/advance，供 make smoke 种植闭环调时。
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

# 依赖已启动且开启 debug 调时的 farm-server（make run 默认开启）。
smoke:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/smoke

run-all:
	./scripts/run.sh

stop-all:
	./scripts/stop.sh
