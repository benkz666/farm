COMPOSE_FILE := deploy/compose.yml

.PHONY: infra-up compose-up compose-down migrate run stop api-docs api-docs-check \
	run-gateway run-farm run-social client-dev test proto proto-check \
	smoke smoke-friends smoke-room smoke-help smoke-steal smoke-all gen gen-check \
	obs-up obs-down api-baseline api-ladder service-bench-build bench-fixture-reset

# 生成 OpenAPI / AsyncAPI / gRPC 离线文档。首次运行需先在 docs/api 执行 npm ci。
api-docs:
	npm --prefix docs/api run build

# 验证契约及已提交静态站点没有漂移。
api-docs-check:
	npm --prefix docs/api run check

# OPERATION=all|syncFarm|friendList...；HTTP 场景同时设置 PROTOCOL=http。
api-baseline:
	PROTOCOL="$${PROTOCOL:-ws}" bench/api/run-baseline.sh "$${OPERATION:-all}"

api-ladder:
	PROTOCOL="$${PROTOCOL:-ws}" bench/api/run-ladder.sh "$${OPERATION:?set OPERATION}"

# 构建服务边界压测工具和隔离 Gateway 下游所需的确定性替身。
service-bench-build:
	mkdir -p .run/service-bench/bin
	cd server && go build -o ../.run/service-bench/bin/servicebench ./cmd/servicebench
	cd server && go build -o ../.run/service-bench/bin/benchstub ./cmd/benchstub

# 复用既有账号/token，仅重置业务状态；例如：
# make bench-fixture-reset PROFILE=water FIXTURE=/fixtures/hot-write-15000x18.json
bench-fixture-reset:
	PROFILE="$${PROFILE:?set PROFILE}" FIXTURE="$${FIXTURE:?set FIXTURE}" ./scripts/reset-bench-fixture.sh

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
	cd server && BUF_CACHE_DIR=/tmp/farm-buf-cache PATH=/tmp/farm-bin:$$PATH buf generate
	cd server && BUF_CACHE_DIR=/tmp/farm-buf-cache PATH=/tmp/farm-bin:$$PATH buf generate proto/farm/public/v3/client.proto --template buf.gen.client.yaml

# 验证提交的 proto 生成物没有漂移。
proto-check:
	@set -e; \
	ROOT="$$(pwd)"; \
	TMP=$$(mktemp -d); \
	trap 'rm -rf "$$TMP"' EXIT; \
	cp -R "$$ROOT/server/proto" "$$TMP/proto"; \
	cp "$$ROOT/server/buf.yaml" "$$ROOT/server/buf.gen.yaml" "$$TMP/"; \
	(cd "$$TMP" && BUF_CACHE_DIR=/tmp/farm-buf-cache PATH=/tmp/farm-bin:$$PATH buf generate); \
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

# 启动 Prometheus、Grafana 及各类 exporter，不要求 app profile。
obs-up:
	docker compose -f $(COMPOSE_FILE) --profile obs up -d

# 只停止观测服务，保留 mysql/redis 及 app profile 服务。
obs-down:
	docker compose -f $(COMPOSE_FILE) --profile obs stop prometheus grafana mysqld-exporter redis-exporter cadvisor

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
