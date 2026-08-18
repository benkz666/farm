#!/usr/bin/env bash
# 重置三份合法状态夹具后，重启热状态服务并执行一次单接口边界探测。
set -euo pipefail

profile="${1:?用法: run_quick_stateful_probe.sh <profile> <scenario> <qps> [duration]}"
scenario="${2:?缺少scenario}"
qps="${3:?缺少QPS}"
duration="${4:-2s}"
namespace="${NAMESPACE:-benkz}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
result_root="${RESULT_ROOT:-${repo_root}/bench/results/quick-interface-extreme-20260816}"
reset_concurrency="${RESET_CONCURRENCY:-32}"

wait_journal_idle() {
  local timeout_seconds="${1:-180}"
  local deadline=$((SECONDS + timeout_seconds))
  while ((SECONDS < deadline)); do
    local pending
    pending="$(kubectl -n "$namespace" exec statefulset/redis -- redis-cli GET farm:journal:projection:pending 2>/dev/null | tr -d '\r')"
    if [[ -z "$pending" || "$pending" == "0" ]]; then
      return 0
    fi
    sleep 1
  done
  printf '等待Journal排空超时\n' >&2
  return 1
}

mapfile -t k6_pods < <(
  kubectl -n "$namespace" get pods -l app.kubernetes.io/name=k6 \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | sort
)
if [[ "${#k6_pods[@]}" != 3 ]]; then
  printf '需要3个发压Pod，实际得到%s个\n' "${#k6_pods[@]}" >&2
  exit 1
fi

wait_journal_idle 300
mysql_dsn="$(kubectl -n "$namespace" get secret farm-secrets -o jsonpath='{.data.mysql-dsn}' | base64 -d)"
reset_pids=()
for shard in 0 1 2; do
  fixture="/fixtures/vertical-unit-1x-shard-${shard}.json"
  kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- benchfixture \
    -mysql-dsn "$mysql_dsn" -redis-addr redis:6379 \
    -concurrency "$reset_concurrency" -profile "$profile" -time-profile authentic \
    -reset-input "$fixture" \
    >"${result_root}/reset-${scenario}-${qps}-shard-${shard}.log" 2>&1 &
  reset_pids+=("$!")
done
unset mysql_dsn
status=0
for pid in "${reset_pids[@]}"; do
  wait "$pid" || status=1
done
if [[ "$status" != 0 ]]; then
  printf '夹具重置失败，见%s中的reset日志\n' "$result_root" >&2
  exit 1
fi

kubectl -n "$namespace" rollout restart deployment/gateway deployment/farm >/dev/null
kubectl -n "$namespace" rollout status deployment/gateway --timeout=5m >/dev/null
kubectl -n "$namespace" rollout status deployment/farm --timeout=5m >/dev/null
wait_journal_idle 300

RESULT_ROOT="$result_root" WARMUP_CONCURRENCY="${WARMUP_CONCURRENCY:-128}" \
  "${repo_root}/bench/api/run_quick_single_probe.sh" "$scenario" "$qps" "$duration" full 0
