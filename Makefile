COMPOSE_FILE := deploy/compose.yml

.PHONY: infra-up compose-up compose-down migrate run stop \
	run-gateway run-farm run-social client-dev test proto proto-check \
	smoke smoke-friends smoke-room smoke-help smoke-steal smoke-all gen gen-check

# 从 config/*.csv 生成服务端与前端共享配置。
gen:
	cd tools && go run ./gen-config -root ..
	gofmt -w server/shared/gameconfig/gen_crops.go

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
	cmp -s "$$TMP/a.go" "$$ROOT/server/shared/gameconfig/gen_crops.go"; \
	cmp -s "$$TMP/a.js" "$$ROOT/client/src/game/gen/crops.js"; \
	(cd "$$ROOT/tools" && go test ./gen-config/ -count=1); \
	echo "gen-check: OK"

# 从 proto 生成 gRPC 代码到 server/gen/。
proto:
	cd server && PATH=/tmp/farm-bin:$$PATH buf generate

# 验证提交的 proto 生成物没有漂移。
proto-check:
	@set -e; \
	ROOT="$$(pwd)"; \
	TMP=$$(mktemp -d); \
	trap 'rm -rf "$$TMP"' EXIT; \
	cp -R "$$ROOT/server/proto" "$$TMP/proto"; \
	cp "$$ROOT/server/buf.yaml" "$$ROOT/server/buf.gen.yaml" "$$TMP/"; \
	(cd "$$TMP" && PATH=/tmp/farm-bin:$$PATH buf generate); \
	diff -ru "$$TMP/gen" "$$ROOT/server/gen"; \
	echo "proto-check: OK"

# 只启动本地依赖。
infra-up:
	docker compose -f $(COMPOSE_FILE) up -d mysql redis

# 容器化启动迁移任务、三个后端、Web 及依赖。
compose-up:
	docker compose -f $(COMPOSE_FILE) --profile app up -d --build

compose-down:
	docker compose -f $(COMPOSE_FILE) --profile app down

migrate:
	docker compose -f $(COMPOSE_FILE) --profile app run --rm migrate

# 默认开发入口始终启动 Gateway/Farm/Social，不存在 all 模式。
run:
	./scripts/run.sh

stop:
	./scripts/stop.sh

run-gateway run-farm run-social:
	@service="$(@:run-%=%)"; \
	set -a; [ ! -f .env ] || . ./.env; set +a; \
	case "$$service" in \
		gateway) pkg=./cmd/gateway ;; \
		farm) pkg=./cmd/farmsvr ;; \
		social) pkg=./cmd/socialsvr ;; \
		*) echo "unknown service: $$service" >&2; exit 1 ;; \
	esac; \
	cd server && go run "$$pkg"

client-dev:
	cd client && npm run dev

test:
	cd server && go test ./...

smoke:
	cd server && go run ./cmd/farmctl

smoke-friends:
	cd server && go run ./cmd/farmctl friends

smoke-room:
	cd server && go run ./cmd/farmctl room

smoke-help:
	cd server && go run ./cmd/farmctl help

smoke-steal:
	cd server && go run ./cmd/farmctl steal

smoke-all:
	cd server && go run ./cmd/farmctl all
