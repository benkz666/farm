#!/usr/bin/env bash
# 一键启动：检查依赖 → MySQL/Redis → 迁移 → farm-server(:9002) → Vite(:9001)
# 固定端口；若被占用则结束占用进程后启动。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

RUN_DIR="$ROOT/.run"
LOG_DIR="$RUN_DIR/logs"
mkdir -p "$LOG_DIR"

SERVER_PID_FILE="$RUN_DIR/farm-server.pid"
VITE_PID_FILE="$RUN_DIR/vite.pid"
SERVER_LOG="$LOG_DIR/farm-server.log"
VITE_LOG="$LOG_DIR/vite.log"

# 固定端口（不可配置覆盖，保证团队一致）
VITE_PORT=9001
HTTP_PORT=9002

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
  die "${label} 未就绪 (最后 HTTP ${code}): ${url} -- 见 ${SERVER_LOG} / ${VITE_LOG}"
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

info "固定端口: 前端 ${VITE_PORT} / 后端 ${HTTP_PORT}"
stop_pid_file "$VITE_PID_FILE" "Vite"
stop_pid_file "$SERVER_PID_FILE" "farm-server"
pkill -f 'cmd/farm-server' 2>/dev/null || true
pkill -f "vite.*--port ${VITE_PORT}" 2>/dev/null || true
kill_port "$VITE_PORT"
kill_port "$HTTP_PORT"

ensure_frontend_deps
ensure_backend_deps

info "启动 MySQL + Redis"
docker compose -f deploy/compose.yml up -d

info "等待 MySQL healthy"
for i in $(seq 1 60); do
  if docker compose -f deploy/compose.yml exec -T mysql mysqladmin ping -h 127.0.0.1 -ufarm -pfarm --silent >/dev/null 2>&1; then
    break
  fi
  sleep 1
  [[ "$i" -eq 60 ]] && die "MySQL 未就绪"
done

info "执行数据库迁移"
docker compose -f deploy/compose.yml exec -T mysql \
  mysql -ufarm -pfarm farm < server/migrations/001_init.sql
docker compose -f deploy/compose.yml exec -T mysql \
  mysql -ufarm -pfarm farm < server/migrations/002_items.sql
docker compose -f deploy/compose.yml exec -T mysql \
  mysql -ufarm -pfarm farm < server/migrations/003_friendship.sql

info "启动 farm-server → :${HTTP_PORT}"
(
  set -a
  # shellcheck disable=SC1091
  . "$ROOT/.env"
  set +a
  export FARM_HTTP_ADDR=":${HTTP_PORT}"
  export FARM_ALLOW_DEBUG_TIME=1
  cd "$ROOT/server"
  exec go run ./cmd/farm-server
) >"$SERVER_LOG" 2>&1 &
echo $! >"$SERVER_PID_FILE"

info "启动 Vite → :${VITE_PORT}"
(
  cd "$ROOT/client"
  exec npm run dev -- --host 0.0.0.0 --port "${VITE_PORT}" --strictPort
) >"$VITE_LOG" 2>&1 &
echo $! >"$VITE_PID_FILE"

info "等待服务就绪"
wait_http "http://127.0.0.1:${HTTP_PORT}/api/login" "400" "farm-server" 50 "POST"
wait_http "http://127.0.0.1:${VITE_PORT}/" "200" "Vite" 50 "GET"

cat <<EOF

启动完成。

  前端:  http://127.0.0.1:${VITE_PORT}/
  后端:  http://127.0.0.1:${HTTP_PORT}/
  日志:  ${LOG_DIR}/
  停止:  ./scripts/stop.sh

登录入口：http://127.0.0.1:${VITE_PORT}/login （右下角 Net 诊断仅 DEV 用）。

EOF
