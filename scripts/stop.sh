#!/usr/bin/env bash
# 停止 scripts/run.sh 启动的五个后端服务与 Vite；默认保留基础设施容器。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_DIR="$ROOT/.run"

info() { printf '==> %s\n' "$*"; }

stop_pid_file() {
  local pid_file="$1"
  local label="$2"
  if [[ ! -f "$pid_file" ]]; then
    info "${label}: 无 pid 文件，跳过"
    return 0
  fi
  local pid=""
  pid="$(<"$pid_file")"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    info "停止 ${label} (pid ${pid})"
    kill "$pid" 2>/dev/null || true
    sleep 0.5
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$pid_file"
}

stop_pid_file "$RUN_DIR/vite.pid" "Vite"
for service in gateway farm worker social auth; do
  stop_pid_file "$RUN_DIR/${service}.pid" "$service"
done

if [[ "${1:-}" == "--compose" ]] || [[ "${1:-}" == "-c" ]]; then
  info "停止基础设施容器"
  docker compose -f "$ROOT/deploy/compose.yml" --profile app down
fi

info "已关闭；如需同时关闭 MySQL/Redis/Kafka，请使用 ./scripts/stop.sh --compose"
