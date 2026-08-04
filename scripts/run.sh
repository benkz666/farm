#!/usr/bin/env bash
# 本地开发一键启动：基础设施 → 迁移 → 三个 Go 热重载服务 → Vite HMR。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ $# -ne 0 ]]; then
  printf 'usage: %s\n' "$0" >&2
  printf '当前只保留三服务模式，不再接受 all/shards 参数。\n' >&2
  exit 2
fi

RUN_DIR="$ROOT/.run"
LOG_DIR="$RUN_DIR/logs"
mkdir -p "$LOG_DIR"

VITE_PORT=9001
GATEWAY_PORT=9002
SOCIAL_PORT=9004
FARM_PORT=9100
GATEWAY_GRPC_PORT=9202
SOCIAL_GRPC_PORT=9204
FARM_GRPC_PORT=9210

MYSQL_HOST="${FARM_MYSQL_HOST:-127.0.0.1}"
MYSQL_USER="${FARM_MYSQL_USER:-farm}"
MYSQL_PASSWORD="${FARM_MYSQL_PASSWORD:-farm}"
MYSQL_DATABASE="${FARM_MYSQL_DATABASE:-farm}"

info() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1（请先安装）"
}

load_env() {
  set -a
  # shellcheck disable=SC1091
  . "$ROOT/.env"
  set +a
  # 兼容尚未补充新变量的旧本地 .env；该脚本只用于 dev 编排。
  export FARM_ENV="${FARM_ENV:-dev}"
  if [[ -z "${FARM_INTERNAL_TOKEN:-}" ]]; then
    if [[ "$FARM_ENV" != "dev" ]]; then
      die "非 dev 环境必须显式配置 FARM_INTERNAL_TOKEN"
    fi
    export FARM_INTERNAL_TOKEN=dev-only-internal-token-change-me
  fi
  if [[ "$FARM_ENV" == "dev" ]]; then
    export FARM_ALLOW_DEBUG_TIME="${FARM_ALLOW_DEBUG_TIME:-1}"
  fi
}

kill_port() {
  local port="$1"
  local pids=""
  pids="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)"
  [[ -z "$pids" ]] && return 0
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
  local pid_file="$1"
  local label="$2"
  [[ -f "$pid_file" ]] || return 0
  local pid=""
  pid="$(<"$pid_file")"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    info "停止旧的 ${label} (pid ${pid})"
    kill "$pid" 2>/dev/null || true
    sleep 0.3
  fi
  rm -f "$pid_file"
}

wait_http() {
  local url="$1"
  local expected="$2"
  local label="$3"
  local method="${4:-GET}"
  local code=""
  for _ in $(seq 1 60); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 1 -X "$method" "$url" 2>/dev/null || true)"
    [[ "$code" == "$expected" ]] && return 0
    sleep 0.5
  done
  die "${label} 未就绪（最后 HTTP ${code}）：${url}，请检查 ${LOG_DIR}/${label}.log"
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
  local version=""
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
  '

  while IFS= read -r migration; do
    version="${migration##*/}"
    [[ "$version" =~ ^[0-9]{3}_[A-Za-z0-9_]+\.sql$ ]] || die "迁移文件名不合法：$migration"
    recorded="$(mysql_client --batch --skip-column-names \
      -e "SELECT 1 FROM schema_migrations WHERE version = '${version}' LIMIT 1" </dev/null 2>/dev/null || true)"
    if [[ "$recorded" == "1" ]]; then
      ((skipped += 1))
      continue
    fi
    info "应用迁移: ${migration#server/migrations/}"
    mysql_client < "$migration" || die "迁移失败：$migration"
    mysql_client -e "
      INSERT INTO schema_migrations (version, applied_at)
      VALUES ('${version}', CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3)) * 1000 AS UNSIGNED));
    "
    ((applied += 1))
  done < <(find server/migrations -type f -name '*.sql' | sort -t/ -k4,4)
  info "数据库迁移完成：新应用 ${applied} 个，已跳过 ${skipped} 个"
}

ensure_dependencies() {
  if [[ ! -d client/node_modules/vite ]] || [[ ! -d client/node_modules/vue ]]; then
    info "安装前端依赖"
    (cd client && npm install)
  else
    info "前端依赖已就绪"
  fi
  info "同步 Go 模块依赖"
  (cd server && go mod download)
}

start_service() {
  local service="$1"
  local pkg=""
  case "$service" in
    gateway) pkg="./cmd/gateway" ;;
    farm) pkg="./cmd/farmsvr" ;;
    social) pkg="./cmd/socialsvr" ;;
    *) die "unknown service: ${service}" ;;
  esac
  local log_file="$LOG_DIR/${service}.log"
  local pid_file="$RUN_DIR/${service}.pid"
  info "启动 ${service}"
  : >"$log_file"
  (
    load_env
    exec python3 "$ROOT/scripts/spawn-detached.py" \
      --cwd "$ROOT" \
      --log-file "$log_file" \
      --pid-file "$pid_file" \
      -- python3 "$ROOT/scripts/watch-go.py" \
        --root "$ROOT/server" \
        --package "$pkg" \
        --label "$service"
  ) >/dev/null
}

for command in docker go node npm python3 curl lsof find sort; do
  need_cmd "$command"
done

if [[ ! -f .env ]]; then
  info "创建 .env（从 .env.example）"
  cp .env.example .env
fi
load_env

# compose 应用容器会占用同一组端口。开发模式只保留 MySQL/Redis，
# 后端与前端均直接从工作区启动，代码保存后无需重新 build/deploy。
info "停止可能占用开发端口的 compose 应用容器"
docker compose -f deploy/compose.yml --profile app stop web gateway farm social >/dev/null 2>&1 || true

for service in gateway social farm; do
  stop_pid_file "$RUN_DIR/${service}.pid" "$service"
done
stop_pid_file "$RUN_DIR/vite.pid" "Vite"
for port in "$VITE_PORT" "$GATEWAY_PORT" "$GATEWAY_GRPC_PORT" "$SOCIAL_PORT" "$SOCIAL_GRPC_PORT" "$FARM_PORT" "$FARM_GRPC_PORT"; do
  kill_port "$port"
done

ensure_dependencies

info "启动 MySQL + Redis"
docker compose -f deploy/compose.yml up -d mysql redis
info "等待 MySQL healthy"
for attempt in $(seq 1 60); do
  mysql_admin ping --silent >/dev/null 2>&1 && break
  [[ "$attempt" -eq 60 ]] && die "MySQL 未就绪"
  sleep 1
done
run_migrations

# 下游服务先就绪，Gateway 最后接收外部流量。
start_service social
wait_http "http://127.0.0.1:9304/readyz" "200" "social"

start_service farm
wait_http "http://127.0.0.1:9310/readyz" "200" "farm"
start_service gateway
wait_http "http://127.0.0.1:${GATEWAY_PORT}/api/login" "400" "gateway" POST

info "启动 Vite"
: >"$LOG_DIR/vite.log"
(
  export FARM_GATEWAY_URL="http://127.0.0.1:${GATEWAY_PORT}"
  exec python3 "$ROOT/scripts/spawn-detached.py" \
    --cwd "$ROOT/client" \
    --log-file "$LOG_DIR/vite.log" \
    --pid-file "$RUN_DIR/vite.pid" \
    -- npm run dev -- --host 0.0.0.0 --port "$VITE_PORT" --strictPort
) >/dev/null
wait_http "http://127.0.0.1:${VITE_PORT}/" "200" "vite"

cat <<EOF

启动完成（源码热重载开发模式）。

  前端:    http://127.0.0.1:${VITE_PORT}/
  Gateway: http://127.0.0.1:${GATEWAY_PORT}/
  Social:  gRPC 127.0.0.1:${SOCIAL_GRPC_PORT}
  Farm:    gRPC 127.0.0.1:${FARM_GRPC_PORT}

日志目录: ${LOG_DIR}

保存 client/src 下的文件会由 Vite HMR 立即更新；
保存 server 下的 Go 文件会自动重新编译并重启三个后端服务。
无需重新执行 docker compose build/up。
EOF
