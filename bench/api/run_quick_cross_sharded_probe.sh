#!/usr/bin/env bash
# 使用互不重叠的账号区间，将跨农场单接口压力均匀绑定到三个Gateway。
set -euo pipefail

profile="${1:?用法: run_quick_cross_sharded_probe.sh <profile> <operation> <qps> [duration]}"
operation="${2:?缺少operation}"
total_qps="${3:?缺少QPS}"
duration="${4:-3s}"
namespace="${NAMESPACE:-benkz}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
result_root="${RESULT_ROOT:-${repo_root}/bench/results/quick-interface-extreme-20260816}"
fixture="/fixtures/cross-extreme-6000.json"
run_dir="${result_root}/${operation}-qps-${total_qps}"
mkdir -p "$run_dir"

mapfile -t k6_pods < <(kubectl -n "$namespace" get pods -l app.kubernetes.io/name=k6 -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | sort)
if [[ "${#k6_pods[@]}" != 3 ]]; then
  printf '需要3个发压Pod\n' >&2
  exit 1
fi

if [[ "${SKIP_FIXTURE_RESET:-0}" != 1 ]]; then
  mysql_dsn="$(kubectl -n "$namespace" get secret farm-secrets -o jsonpath='{.data.mysql-dsn}' | base64 -d)"
  kubectl -n "$namespace" exec "${k6_pods[0]}" -- benchfixture \
    -mysql-dsn "$mysql_dsn" -redis-addr redis:6379 -concurrency 32 \
    -profile "$profile" -time-profile authentic -reset-input "$fixture" \
    >"${run_dir}/reset.log" 2>&1
  unset mysql_dsn
fi

kubectl -n "$namespace" rollout restart deployment/gateway deployment/farm >/dev/null
kubectl -n "$namespace" rollout status deployment/gateway --timeout=5m >/dev/null
kubectl -n "$namespace" rollout status deployment/farm --timeout=5m >/dev/null
mapfile -t gateway_ips < <(kubectl -n "$namespace" get pods -l app.kubernetes.io/name=gateway -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.status.podIP}}{{"\n"}}{{end}}{{end}}' | sort)
if [[ "${#gateway_ips[@]}" != 3 ]]; then
  printf '需要3个Gateway\n' >&2
  exit 1
fi

pids=()
for shard in 0 1 2; do
  shard_qps=$((total_qps / 3))
  ((shard < total_qps % 3)) && shard_qps=$((shard_qps + 1))
  remote="/tmp/cross-${operation}-${total_qps}-${shard}.json"
  kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- servicebench \
    -mode gateway-ws -accounts "$fixture" -fixture-account-offset "$((shard * 2000))" \
    -gateway-urls "ws://${gateway_ips[$shard]}:9002/ws" -operation "$operation" \
    -qps "$shard_qps" -duration "$duration" -concurrency 2000 -fixed-connections 2000 \
    -warmup-concurrency 256 -warmup-settle 1s -warmup-mode full -output "$remote" \
    >"${run_dir}/shard-${shard}.log" 2>&1 &
  pids+=("$!")
done
status=0
for pid in "${pids[@]}"; do wait "$pid" || status=1; done
if [[ "$status" != 0 ]]; then
  printf '跨农场分片测试失败，见%s\n' "$run_dir" >&2
  exit 1
fi
for shard in 0 1 2; do
  kubectl -n "$namespace" cp "${k6_pods[$shard]}:/tmp/cross-${operation}-${total_qps}-${shard}.json" "${run_dir}/shard-${shard}.json" >/dev/null
done

python3 - "$run_dir" "$operation" "$total_qps" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
rows = [json.loads(p.read_text()) for p in sorted(path.glob('shard-*.json'))]
sent = sum(r['sent'] for r in rows); ok = sum(r['succeeded'] for r in rows); failed = sum(r['failed'] for r in rows)
summary = {
  'scenario': sys.argv[2], 'target_qps': int(sys.argv[3]),
  'actual_qps': sum(r['actual_qps'] for r in rows), 'sent': sent, 'succeeded': ok, 'failed': failed,
  'success_rate': ok / sent if sent else 0,
  'average_ms': sum(r['average_ms'] * r['succeeded'] for r in rows) / ok if ok else None,
  'p90_ms': max(r['p90_ms'] for r in rows), 'p99_ms': max(r['p99_ms'] for r in rows),
  'max_ms': max(r['max_ms'] for r in rows),
}
(path / 'summary.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2) + '\n')
print(json.dumps(summary, ensure_ascii=False))
PY
