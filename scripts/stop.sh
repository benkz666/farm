#!/usr/bin/env bash
# 停止由 scripts/run.sh 拉起的进程（默认不关 compose）
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_DIR="$ROOT/.run"
SERVER_PID_FILE="$RUN_DIR/farm-server.pid"
VITE_PID_FILE="$RUN_DIR/vite.pid"
VITE_PORT=9001
HTTP_PORT=9002
FARM0_PORT=9100
FARM1_PORT=9101
GW0_PORT=9200
GW1_PORT=9201

COMPOSE_DOWN=0
if [[ "${1:-}" == "--compose" ]] || [[ "${1:-}" == "-c" ]]; then
  COMPOSE_DOWN=1
fi

info() { printf '==> %s\n' "$*"; }

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
  info "释放端口 ${port} (pids: ${pids})"
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
    info "${label}: 无 pid 文件，跳过"
    return 0
  fi
  pid="$(cat "$file" 2>/dev/null || true)"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    info "停止 ${label} (pid ${pid})"
    kill "$pid" 2>/dev/null || true
    sleep 0.5
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null || true
    fi
  else
    info "${label}: 进程已不在"
  fi
  rm -f "$file"
}

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

if [[ "$COMPOSE_DOWN" -eq 1 ]]; then
  info "停止 compose（含 shards profile 服务）"
  docker compose -f "$ROOT/deploy/compose.yml" --profile shards down
fi

info "已关闭，如需关闭数据库: ./scripts/stop.sh --compose"
