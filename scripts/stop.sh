#!/usr/bin/env bash
# 停止本地源码开发环境；保留 Docker 容器、数据库卷与数据，不影响 k3s。
# 支持用户以 `sh scripts/stop.sh` 或在 scripts 目录中执行 `sh stop.sh`。
if [ -z "${BASH_VERSION:-}" ]; then
  exec bash "$0" "$@"
fi
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_DIR="$ROOT/.run"

info() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

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
    for _ in {1..20}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.2
    done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$pid_file"
}

stop_source_processes() {
  stop_pid_file "$RUN_DIR/vite.pid" "Vite"
  for service in gateway farm social; do
    stop_pid_file "$RUN_DIR/${service}.pid" "$service"
  done
}

stop_compose_services() {
  if ! command -v docker >/dev/null 2>&1; then
    warn "未找到 Docker；源码进程已停止，中间件未处理"
    return 0
  fi
  info "停止本项目的 Docker Compose 服务（保留容器和数据）"
  docker compose -f "$ROOT/deploy/compose.yml" --profile app stop
}

if [[ $# -ne 0 ]]; then
  die "usage: $0（无需参数；仅停止本地源码开发环境）"
fi

info "停止本地源码开发环境（不影响 k3s）"
stop_source_processes
stop_compose_services
info "已停止；Docker 容器和数据库数据均已保留"
