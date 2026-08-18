#!/usr/bin/env bash
# 在三个独立发压Pod上并行执行一次单接口快速探测，并生成保守聚合结果。
set -euo pipefail

scenario="${1:?用法: run_quick_single_probe.sh <scenario> <qps> <duration> [warmup-mode] [requests-per-account]}"
total_qps="${2:?缺少总QPS}"
duration="${3:?缺少持续时间，例如3s}"
warmup_mode="${4:-full}"
requests_per_account="${5:-0}"
warmup_concurrency="${WARMUP_CONCURRENCY:-128}"
namespace="${NAMESPACE:-benkz}"
result_root="${RESULT_ROOT:-/data/workspace/farm/bench/results/quick-interface-20260816}"
run_dir="${result_root}/${scenario}-qps-${total_qps}"

if ! [[ "$total_qps" =~ ^[1-9][0-9]*$ && "$requests_per_account" =~ ^[0-9]+$ ]]; then
  printf 'QPS必须为正整数，requests-per-account必须为非负整数\n' >&2
  exit 2
fi

mkdir -p "$run_dir"
mapfile -t load_pods < <(
  kubectl -n "$namespace" get pods -l app.kubernetes.io/name=k6 \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | sort
)
mapfile -t gateway_ips < <(
  kubectl -n "$namespace" get pods -l app.kubernetes.io/name=gateway \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.status.podIP}}{{"\n"}}{{end}}{{end}}' | sort
)
internal_token="$(
  kubectl -n "$namespace" get secret farm-secrets \
    -o jsonpath='{.data.internal-token}' | base64 -d
)"
if [[ "${#load_pods[@]}" != 3 || "${#gateway_ips[@]}" != 3 ]]; then
  printf '需要3个发压Pod和3个Gateway，实际为%s和%s\n' "${#load_pods[@]}" "${#gateway_ips[@]}" >&2
  exit 1
fi

pids=()
for shard in 0 1 2; do
  shard_qps=$((total_qps / 3))
  ((shard < total_qps % 3)) && shard_qps=$((shard_qps + 1))
  remote_output="/tmp/quick-${scenario}-${total_qps}-${shard}.json"
  fixture="/fixtures/vertical-unit-1x-shard-${shard}.json"
  if [[ "$scenario" == "handshake" ]]; then
    kubectl -n "$namespace" exec "${load_pods[$shard]}" -- servicebench \
      -mode gateway-handshake -accounts "$fixture" \
      -gateway-urls "ws://${gateway_ips[$shard]}:9002/ws" \
      -qps "$shard_qps" -duration "$duration" -concurrency 16261 \
      -output "$remote_output" >"${run_dir}/shard-${shard}.log" 2>&1 &
  elif [[ "$scenario" == "are-friends" ]]; then
    uid_base=$((26000000 + shard * 32521))
    kubectl -n "$namespace" exec "${load_pods[$shard]}" -- servicebench \
      -mode social-are-friends -target social:9204 \
      -token "$internal_token" \
      -qps "$shard_qps" -duration "$duration" -concurrency 4096 \
      -uid-base "$uid_base" -uid-count 16261 \
      -requests-per-account "$requests_per_account" \
      -output "$remote_output" >"${run_dir}/shard-${shard}.log" 2>&1 &
  else
    kubectl -n "$namespace" exec "${load_pods[$shard]}" -- servicebench \
      -mode gateway-ws -operation "$scenario" -accounts "$fixture" \
      -gateway-urls "ws://${gateway_ips[$shard]}:9002/ws" \
      -qps "$shard_qps" -duration "$duration" -concurrency 16261 \
      -warmup-concurrency "$warmup_concurrency" -warmup-settle 1s \
      -warmup-mode "$warmup_mode" -requests-per-account "$requests_per_account" \
      -output "$remote_output" >"${run_dir}/shard-${shard}.log" 2>&1 &
  fi
  pids+=("$!")
done

status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done
if [[ "$status" != 0 ]]; then
  printf '接口%s在%s QPS探测失败，见%s中的分片日志\n' "$scenario" "$total_qps" "$run_dir" >&2
  exit 1
fi

for shard in 0 1 2; do
  kubectl -n "$namespace" cp \
    "${load_pods[$shard]}:/tmp/quick-${scenario}-${total_qps}-${shard}.json" \
    "${run_dir}/shard-${shard}.json" >/dev/null
done

python3 - "$run_dir" "$scenario" "$total_qps" <<'PY'
import json
import pathlib
import sys

run_dir = pathlib.Path(sys.argv[1])
scenario = sys.argv[2]
target_qps = int(sys.argv[3])
rows = [json.loads(path.read_text(encoding="utf-8")) for path in sorted(run_dir.glob("shard-*.json"))]
sent = sum(int(row["sent"]) for row in rows)
succeeded = sum(int(row["succeeded"]) for row in rows)
failed = sum(int(row["failed"]) for row in rows)
weighted_avg = (
    sum(float(row["average_ms"]) * int(row["succeeded"]) for row in rows) / succeeded
    if succeeded else None
)
summary = {
    "scenario": scenario,
    "target_qps": target_qps,
    "actual_qps": sum(float(row["actual_qps"]) for row in rows),
    "sent": sent,
    "succeeded": succeeded,
    "failed": failed,
    "success_rate": succeeded / sent if sent else 0,
    "average_ms": weighted_avg,
    "p90_ms": max(float(row["p90_ms"]) for row in rows),
    "p99_ms": max(float(row["p99_ms"]) for row in rows),
    "max_ms": max(float(row["max_ms"]) for row in rows),
    "delivery_ratio": succeeded / (target_qps * max(float(row["wall_millis"]) for row in rows) / 1000),
    "aggregation": "weighted Avg; conservative maximum of shard P90/P99",
}
(run_dir / "summary.json").write_text(
    json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
print(json.dumps(summary, ensure_ascii=False))
PY
