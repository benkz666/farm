#!/usr/bin/env bash
# 本地开发一键启动：基础设施 → 迁移 → 三个 Go 热重载服务 → Vite HMR。
# 支持用户以 `sh scripts/run.sh` 或在 scripts 目录中执行 `sh run.sh`。
# 本脚本使用了 Bash 的 `[[ ... ]]` 与进程替换，非 Bash 时先重启到正确解释器。
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

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
  local migrations=""
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

  migrations="$(find server/migrations -type f -name '*.sql' | sort -t/ -k4,4)"
  while IFS= read -r migration; do
    [[ -z "$migration" ]] && continue
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
    " </dev/null
    ((applied += 1))
  done <<EOF
${migrations}
EOF
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

stop_local_processes() {
  for service in gateway social farm; do
    stop_pid_file "$RUN_DIR/${service}.pid" "$service"
  done
  stop_pid_file "$RUN_DIR/vite.pid" "Vite"
}

release_app_ports() {
  for port in "$VITE_PORT" "$GATEWAY_PORT" "$GATEWAY_GRPC_PORT" "$SOCIAL_PORT" "$SOCIAL_GRPC_PORT" "$FARM_PORT" "$FARM_GRPC_PORT"; do
    kill_port "$port"
  done
}

start_direct() {
  for command in docker go node npm python3 curl lsof find sort; do
    need_cmd "$command"
  done

  # Compose 应用容器会占用同一组端口。开发模式只保留 MySQL/Redis，
  # 后端与前端均直接从工作区启动，代码保存后无需重新 build/deploy。
  info "停止可能占用开发端口的 compose 应用容器"
  docker compose -f deploy/compose.yml --profile app stop web gateway farm social >/dev/null 2>&1 || true
  stop_local_processes
  release_app_ports
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
EOF
}

start_deploy() {
  for command in docker curl lsof; do
    need_cmd "$command"
  done

  # Compose 是容器化运行，不是 Kubernetes Pod；每次启动都会按当前源码构建镜像。
  info "停止本地热重载进程并释放应用端口"
  stop_local_processes
  release_app_ports
  info "构建并启动 Docker Compose 应用容器"
  docker compose -f deploy/compose.yml --profile app up -d --build --remove-orphans
  wait_http "http://127.0.0.1:${GATEWAY_PORT}/api/login" "400" "gateway" POST
  wait_http "http://127.0.0.1:${VITE_PORT}/" "200" "web"

  cat <<EOF

启动完成（Docker Compose 容器化运行模式）。

  前端:    http://127.0.0.1:${VITE_PORT}/
  Gateway: http://127.0.0.1:${GATEWAY_PORT}/

查看日志:
  docker compose -f deploy/compose.yml --profile app logs -f

代码变更后请重新执行本脚本并选择 2，以重新构建镜像。
EOF
}

choose_mode() {
  local choice="${1:-}"
  case "$choice" in
    1|direct|dev) printf 'direct' ;;
    2|deploy|compose) printf 'deploy' ;;
    '')
      [[ -t 0 ]] || die "非交互环境请显式指定模式：$0 [1|2]（1=直接运行，2=容器化运行）"
      printf '\n请选择启动方式：\n' >&2
      printf '  1) 直接运行（Go 热重载 + Vite HMR，MySQL/Redis 使用容器）\n' >&2
      printf '  2) 容器化运行（Docker Compose 构建并运行全部应用容器）\n' >&2
      read -r -p '输入 1 或 2: ' choice
      choose_mode "$choice"
      ;;
    *) die "无效模式：${choice}（可选 1/direct 或 2/deploy）" ;;
  esac
}

if [[ $# -gt 1 ]]; then
  die "usage: $0 [1|2]"
fi

if [[ ! -f .env ]]; then
  info "创建 .env（从 .env.example）"
  cp .env.example .env
fi
load_env

mode="$(choose_mode "${1:-}")"
case "$mode" in
  direct) start_direct ;;
  deploy) start_deploy ;;
esac
