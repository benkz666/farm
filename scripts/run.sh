#!/usr/bin/env bash
# 一键启动：检查依赖 → MySQL/Redis(/Kafka) → 迁移 → 服务 → Vite(:9001)
#
# 用法：
#   ./scripts/run.sh           # 单进程 FARM_ROLE=all（:9002），开发默认
#   ./scripts/run.sh shards    # 双分片：farm-0/1(:9100/:9101) + gateway-0/1(:9200/:9201)
#
# 固定端口；若被占用则结束占用进程后启动。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

MODE="${1:-all}"
case "$MODE" in
  all|shards) ;;
  *)
    printf 'usage: %s [all|shards]\n' "$0" >&2
    exit 2
    ;;
esac

RUN_DIR="$ROOT/.run"
LOG_DIR="$RUN_DIR/logs"
mkdir -p "$LOG_DIR"

SERVER_PID_FILE="$RUN_DIR/farm-server.pid"
VITE_PID_FILE="$RUN_DIR/vite.pid"
SERVER_LOG="$LOG_DIR/farm-server.log"
VITE_LOG="$LOG_DIR/vite.log"

# 本地 compose 中 MySQL 的开发账号。密码仅通过 docker exec 的进程环境传入，
# 不再拼进 mysql 的 -p 命令行参数，避免终端警告和被进程参数读取。
MYSQL_HOST="${FARM_MYSQL_HOST:-127.0.0.1}"
MYSQL_USER="${FARM_MYSQL_USER:-farm}"
MYSQL_PASSWORD="${FARM_MYSQL_PASSWORD:-farm}"
MYSQL_DATABASE="${FARM_MYSQL_DATABASE:-farm}"

# 固定端口（不可配置覆盖，保证团队一致）
VITE_PORT=9001
HTTP_PORT=9002
FARM0_PORT=9100
FARM1_PORT=9101
GW0_PORT=9200
GW1_PORT=9201

info() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1（请先安装）"
}

kill_port() {
  local port="$1"
  local pids=""
  if ! command -v lsof >/dev/null 2>&1; then
    return 0
  fi
  pids="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)"
  if [[ -z "$pids" ]]; then
    return 0
  fi
  info "端口 ${port} 被占用，结束进程: ${pids}"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  sleep 0.4
  pids="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)"
  if [[ -n "$pids" ]]; then
    # shellcheck disable=SC2086
    kill -9 $pids 2>/dev/null || true
  fi
}

stop_pid_file() {
  local file="$1"
  local label="$2"
  local pid=""
  if [[ ! -f "$file" ]]; then
    return 0
  fi
  pid="$(cat "$file" 2>/dev/null || true)"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    info "停止旧的 ${label} (pid ${pid})"
    kill "$pid" 2>/dev/null || true
    sleep 0.3
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$file"
}

wait_http() {
  local url="$1"
  local expect="$2"
  local label="$3"
  local tries="${4:-40}"
  local method="${5:-GET}"
  local i=""
  local code=""
  for i in $(seq 1 "$tries"); do
    if [[ "$method" == "POST" ]]; then
      code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 1 -X POST -H 'content-type: application/json' -d '{}' "$url" 2>/dev/null || true)"
    else
      code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 1 "$url" 2>/dev/null || true)"
    fi
    if [[ "$code" == "$expect" ]]; then
      return 0
    fi
    sleep 0.5
  done
  die "${label} 未就绪 (最后 HTTP ${code}): ${url} -- 见 ${LOG_DIR}/"
}

ensure_frontend_deps() {
  if [[ ! -f client/package.json ]]; then
    die "缺少 client/package.json"
  fi
  if [[ ! -d client/node_modules/vite ]] || [[ ! -d client/node_modules/vue ]]; then
    info "安装前端依赖 (npm install)"
    (cd client && npm install)
  else
    info "前端依赖已就绪"
  fi
}

ensure_backend_deps() {
  if [[ ! -f server/go.mod ]]; then
    die "缺少 server/go.mod"
  fi
  info "检查并下载 Go 模块"
  (cd server && go mod download)
}

mysql_client() {
  docker compose -f deploy/compose.yml exec -T \
    -e "MYSQL_PWD=${MYSQL_PASSWORD}" \
    mysql mysql --protocol=TCP -h "$MYSQL_HOST" -u"$MYSQL_USER" "$@" "$MYSQL_DATABASE"
}

mysql_admin() {
  docker compose -f deploy/compose.yml exec -T \
    -e "MYSQL_PWD=${MYSQL_PASSWORD}" \
    mysql mysqladmin --protocol=TCP -h "$MYSQL_HOST" -u"$MYSQL_USER" "$@"
}

run_migrations() {
  local migration=""
  local name=""
  local recorded=""
  local applied=0
  local skipped=0

  info "检查数据库迁移"
  mysql_client -e '
    CREATE TABLE IF NOT EXISTS schema_migrations (
      version VARCHAR(255) NOT NULL,
      applied_at BIGINT NOT NULL,
      PRIMARY KEY (version)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
  ' || die "无法创建迁移记录表"

  for migration in server/migrations/*.sql; do
    name="${migration##*/}"
    [[ "$name" =~ ^[0-9]{3}_[A-Za-z0-9_]+\.sql$ ]] || die "迁移文件名不合法：$name"
    recorded="$(mysql_client --batch --skip-column-names \
      -e "SELECT 1 FROM schema_migrations WHERE version = '${name}' LIMIT 1" 2>/dev/null || true)"
    if [[ "$recorded" == "1" ]]; then
      ((skipped += 1))
      continue
    fi

    info "应用迁移: ${name}"
    mysql_client < "$migration" || die "迁移失败：$migration"
    mysql_client -e "
      INSERT INTO schema_migrations (version, applied_at)
      VALUES ('${name}', CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3)) * 1000 AS UNSIGNED));
    " || die "无法记录已完成迁移：$name"
    ((applied += 1))
  done

  info "数据库迁移完成：新应用 ${applied} 个，已跳过 ${skipped} 个"
}

load_env() {
  set -a
  # shellcheck disable=SC1091
  . "$ROOT/.env"
  set +a
}

start_farm_server() {
  local role="$1"
  local instance="$2"
  local port="$3"
  local pid_file="$4"
  local log_file="$5"
  (
    load_env
    export FARM_ROLE="$role"
    export FARM_INSTANCE_ID="$instance"
    export FARM_HTTP_ADDR=":${port}"
    export FARM_ROUTE_TABLE="${FARM_ROUTE_TABLE:-deploy/route-table.example.json}"
    export FARM_INTERNAL_TOKEN="${FARM_INTERNAL_TOKEN:-dev-only-internal-token-change-me}"
    if [[ -z "${FARM_FARM_URLS:-}" ]]; then
      export FARM_FARM_URLS="{\"farm-0\":\"http://127.0.0.1:${FARM0_PORT}\",\"farm-1\":\"http://127.0.0.1:${FARM1_PORT}\"}"
    fi
    if [[ -z "${FARM_GATEWAY_URLS:-}" ]]; then
      export FARM_GATEWAY_URLS="{\"gateway-0\":\"http://127.0.0.1:${GW0_PORT}\",\"gateway-1\":\"http://127.0.0.1:${GW1_PORT}\"}"
    fi
    export FARM_BUS="${FARM_BUS:-kafka}"
    export FARM_KAFKA_BROKERS="${FARM_KAFKA_BROKERS:-127.0.0.1:9094}"
    if [[ "$role" == "gateway" || "$role" == "all" || "$role" == "farm" ]]; then
      export FARM_ALLOW_DEBUG_TIME=1
    fi
    cd "$ROOT/server"
    exec go run ./cmd/farm-server
  ) >"$log_file" 2>&1 &
  echo $! >"$pid_file"
}

need_cmd docker
need_cmd go
need_cmd node
need_cmd npm
need_cmd curl
need_cmd lsof

if [[ ! -f .env ]]; then
  info "创建 .env（从 .env.example）"
  cp .env.example .env
fi

load_env
ACTIVE_TIME_PROFILE="${FARM_TIME_PROFILE:-demo}"
info "模式: ${MODE}"
info "启动时间档: ${ACTIVE_TIME_PROFILE}（运行后可在设置页热切换，生长中作物按进度换算）"
stop_pid_file "$VITE_PID_FILE" "Vite"
stop_pid_file "$SERVER_PID_FILE" "farm-server"
for role in farm-0 farm-1 gateway-0 gateway-1; do
  stop_pid_file "$RUN_DIR/${role}.pid" "$role"
done
pkill -f 'cmd/farm-server' 2>/dev/null || true
pkill -f "vite.*--port ${VITE_PORT}" 2>/dev/null || true
kill_port "$VITE_PORT"
kill_port "$HTTP_PORT"
kill_port "$FARM0_PORT"
kill_port "$FARM1_PORT"
kill_port "$GW0_PORT"
kill_port "$GW1_PORT"

ensure_frontend_deps
ensure_backend_deps

info "启动 MySQL + Redis + Kafka"
docker compose -f deploy/compose.yml up -d mysql redis kafka

info "等待 MySQL healthy"
for i in $(seq 1 60); do
  if mysql_admin ping --silent >/dev/null 2>&1; then
    break
  fi
  sleep 1
  [[ "$i" -eq 60 ]] && die "MySQL 未就绪"
done

# 按文件名顺序只执行尚未记录的迁移。迁移文件一旦应用不可改名或改内容，
# 需要数据库变更时新增下一个编号文件。
run_migrations

if [[ "$MODE" == "shards" ]]; then
  info "启动双分片：farm-0/:${FARM0_PORT} farm-1/:${FARM1_PORT} gateway-0/:${GW0_PORT} gateway-1/:${GW1_PORT}"
  start_farm_server farm farm-0 "$FARM0_PORT" "$RUN_DIR/farm-0.pid" "$LOG_DIR/farm-0.log"
  start_farm_server farm farm-1 "$FARM1_PORT" "$RUN_DIR/farm-1.pid" "$LOG_DIR/farm-1.log"
  # 等 Farm 内部端口先起来，再起 Gateway，避免首批转发失败。
  sleep 1
  start_farm_server gateway gateway-0 "$GW0_PORT" "$RUN_DIR/gateway-0.pid" "$LOG_DIR/gateway-0.log"
  start_farm_server gateway gateway-1 "$GW1_PORT" "$RUN_DIR/gateway-1.pid" "$LOG_DIR/gateway-1.log"
else
  info "启动 farm-server (all) → :${HTTP_PORT}"
  (
    load_env
    export FARM_HTTP_ADDR=":${HTTP_PORT}"
    # 此脚本是单进程开发入口，不能受 .env 中分布式角色配置影响。
    export FARM_ROLE=all
    export FARM_ALLOW_DEBUG_TIME=1
    cd "$ROOT/server"
    exec go run ./cmd/farm-server
  ) >"$SERVER_LOG" 2>&1 &
  echo $! >"$SERVER_PID_FILE"
fi

info "启动 Vite → :${VITE_PORT}"
(
  cd "$ROOT/client"
  if [[ "$MODE" == "shards" ]]; then
    export FARM_GATEWAY_URL="http://127.0.0.1:${GW0_PORT}"
  fi
  exec npm run dev -- --host 0.0.0.0 --port "${VITE_PORT}" --strictPort
) >"$VITE_LOG" 2>&1 &
echo $! >"$VITE_PID_FILE"

info "等待服务就绪"
if [[ "$MODE" == "shards" ]]; then
  wait_http "http://127.0.0.1:${GW0_PORT}/api/login" "400" "gateway-0" 60 "POST"
  wait_http "http://127.0.0.1:${GW1_PORT}/api/login" "400" "gateway-1" 60 "POST"
else
  wait_http "http://127.0.0.1:${HTTP_PORT}/api/login" "400" "farm-server" 50 "POST"
fi
wait_http "http://127.0.0.1:${VITE_PORT}/" "200" "Vite" 50 "GET"

if [[ "$MODE" == "shards" ]]; then
  cat <<EOF

启动完成（双分片）。

  前端:       http://127.0.0.1:${VITE_PORT}/
  时间档:     ${ACTIVE_TIME_PROFILE}
  Gateway-0:  http://127.0.0.1:${GW0_PORT}/
  Gateway-1:  http://127.0.0.1:${GW1_PORT}/
  Farm-0:     http://127.0.0.1:${FARM0_PORT}/internal/v1/cmd
  Farm-1:     http://127.0.0.1:${FARM1_PORT}/internal/v1/cmd
  日志:       ${LOG_DIR}/
  停止:       ./scripts/stop.sh
  4a smoke:   cd server && go run ./cmd/smoke shards

也可整栈容器化：
  docker compose -f deploy/compose.yml --profile shards up -d --build

EOF
else
  cat <<EOF

启动完成。

  前端:  http://127.0.0.1:${VITE_PORT}/
  后端:  http://127.0.0.1:${HTTP_PORT}/
  时间档: ${ACTIVE_TIME_PROFILE}
  日志:  ${LOG_DIR}/
  停止:  ./scripts/stop.sh

登录入口：http://127.0.0.1:${VITE_PORT}/login （右下角 Net 诊断仅 DEV 用）。

EOF
fi
