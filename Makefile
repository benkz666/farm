COMPOSE_FILE := deploy/compose.yml
GO_PACKAGES := ./api/... ./platform/... ./services/auth/... ./services/farm/... ./services/gateway/... ./services/social/... ./services/worker/... ./tools/...

.PHONY: infra-up compose-up compose-down migrate run stop \
	run-gateway run-auth run-farm run-social run-worker client-dev test \
	smoke smoke-friends smoke-room smoke-help smoke-steal smoke-all gen gen-check

# 从 config/*.csv 生成服务端与前端共享配置。
gen:
	cd tools && go run ./gen-config -root ..
	gofmt -w server/platform/gameconf/gen_crops.go

# 验证生成器确定性以及提交的生成物没有漂移。
gen-check:
	@set -e; \
	ROOT="$$(pwd)"; \
	TMP=$$(mktemp -d); \
	trap 'rm -rf "$$TMP"' EXIT; \
	(cd "$$ROOT/tools" && go run ./gen-config -root "$$ROOT" -out-go "$$TMP/a.go" -out-js "$$TMP/a.js"); \
	(cd "$$ROOT/tools" && go run ./gen-config -root "$$ROOT" -out-go "$$TMP/b.go" -out-js "$$TMP/b.js"); \
	gofmt -w "$$TMP/a.go" "$$TMP/b.go"; \
	cmp -s "$$TMP/a.go" "$$TMP/b.go"; \
	cmp -s "$$TMP/a.js" "$$TMP/b.js"; \
	cmp -s "$$TMP/a.go" "$$ROOT/server/platform/gameconf/gen_crops.go"; \
	cmp -s "$$TMP/a.js" "$$ROOT/client/src/game/gen/crops.js"; \
	(cd "$$ROOT/tools" && go test ./gen-config/ -count=1); \
	echo "gen-check: OK"

# 只启动本地依赖。
infra-up:
	docker compose -f $(COMPOSE_FILE) up -d mysql redis kafka

# 容器化启动迁移任务、五个后端、Web 及依赖。
compose-up:
	docker compose -f $(COMPOSE_FILE) --profile app up -d --build

compose-down:
	docker compose -f $(COMPOSE_FILE) --profile app down

migrate:
	docker compose -f $(COMPOSE_FILE) --profile app run --rm migrate

# 默认开发入口始终启动 Gateway/Auth/Farm/Social/Worker，不存在 all 模式。
run:
	./scripts/run.sh

stop:
	./scripts/stop.sh

run-gateway run-auth run-farm run-social run-worker:
	@service="$(@:run-%=%)"; \
	set -a; [ ! -f .env ] || . ./.env; set +a; \
	cd server && go run "./services/$$service/cmd/$$service"

client-dev:
	cd client && npm run dev

test:
	cd server && go test $(GO_PACKAGES)

smoke:
	cd server && go run ./tools/smoke

smoke-friends:
	cd server && go run ./tools/smoke friends

smoke-room:
	cd server && go run ./tools/smoke room

smoke-help:
	cd server && go run ./tools/smoke help

smoke-steal:
	cd server && go run ./tools/smoke steal

smoke-all:
	cd server && go run ./tools/smoke all
