#!/usr/bin/env bash
# 暂停 scripts/run.sh 启动的服务，不删除容器实例或持久化数据。
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

stop_local_business() {
  stop_pid_file "$RUN_DIR/vite.pid" "Vite"
  for service in gateway farm social; do
    stop_pid_file "$RUN_DIR/${service}.pid" "$service"
  done
}

stop_compose_business() {
  if ! command -v docker >/dev/null 2>&1; then
    warn "未安装 Docker，已仅关闭本地业务进程"
    return 0
  fi
  info "停止 Docker Compose 业务容器（保留 MySQL/Redis）"
  docker compose -f "$ROOT/deploy/compose.yml" --profile app \
    stop web gateway farm social >/dev/null 2>&1 || true
}

close_business() {
  info "关闭业务服务"
  stop_local_business
  stop_compose_business
  info "业务服务已关闭；MySQL/Redis 保持运行"
}

pause_all() {
  command -v docker >/dev/null 2>&1 || die "关闭全部服务需要 Docker"
  close_business
  info "暂停全部 Docker Compose 服务（保留实例和数据）"
  docker compose -f "$ROOT/deploy/compose.yml" --profile app stop
  info "全部服务已暂停；容器实例和数据仍然保留"
}

choose_mode() {
  local choice="${1:-}"
  case "$choice" in
    1|business|service) printf 'business' ;;
    2|all|compose|--compose|-c) printf 'all' ;;
    '')
      [[ -t 0 ]] || die "非交互环境请显式指定模式：$0 [1|2]（1=关闭业务服务，2=关闭全部服务）"
      printf '\n请选择关闭范围：\n' >&2
      printf '  1) 关闭业务服务（前端、Gateway、Farm、Social；保留 MySQL/Redis）\n' >&2
      printf '  2) 暂停全部服务（保留业务容器、MySQL/Redis 实例及数据）\n' >&2
      read -r -p '输入 1 或 2: ' choice
      choose_mode "$choice"
      ;;
    *) die "无效模式：${choice}（可选 1/business 或 2/all）" ;;
  esac
}

if [[ $# -gt 1 ]]; then
  die "usage: $0 [1|2]"
fi

mode="$(choose_mode "${1:-}")"
case "$mode" in
  business) close_business ;;
  all) pause_all ;;
esac
