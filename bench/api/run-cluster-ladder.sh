#!/usr/bin/env bash
# Three-Gateway load step. Run this inside the dedicated 24-core k6 container,
# or on an equivalent isolated load generator. Each k6 process gets one direct
# Gateway target and a disjoint account shard.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OPERATION=${1:?用法: bench/api/run-cluster-ladder.sh <operationId> <target-qps> [duration]}
TARGET_QPS=${2:?缺少总 target-qps}
DURATION=${3:-20s}
DATA_FILE=${DATA_FILE:?必须设置 DATA_FILE}
GATEWAY_URLS=${GATEWAY_URLS:-http://gateway-0:9002,http://gateway-1:9002,http://gateway-2:9002}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-cluster-${OPERATION}-${TARGET_QPS}}
OUT=${RESULT_DIR:-"$ROOT/.run/bench-results/$RUN_ID"}
SCRIPT=${K6_SCRIPT:-"$ROOT/bench/k6/ws_api_baseline.js"}

IFS=',' read -r -a urls <<< "$GATEWAY_URLS"
shard_count=${#urls[@]}
if (( shard_count != 3 )); then
  echo "GATEWAY_URLS 必须正好包含 3 个地址" >&2
  exit 2
fi
mkdir -p "$OUT"

pids=()
for shard in "${!urls[@]}"; do
  shard_qps=$(( TARGET_QPS / shard_count ))
  (( shard < TARGET_QPS % shard_count )) && shard_qps=$(( shard_qps + 1 ))
  shard_vus=$(( (shard_qps + 7) / 8 ))
  SCENARIO="$OPERATION" MODE=load TARGET_QPS="$shard_qps" TARGET_VUS="$shard_vus" DURATION="$DURATION" \
    BASE_URL="${urls[$shard]}" DATA_FILE="$DATA_FILE" \
    FIXTURE_SHARD_INDEX="$shard" FIXTURE_SHARD_COUNT="$shard_count" \
    k6 run --no-thresholds --summary-export "$OUT/shard-$shard.json" "$SCRIPT" \
    >"$OUT/shard-$shard.log" 2>&1 &
  pids+=("$!")
done

status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done

python3 - "$OUT" "$OPERATION" "$TARGET_QPS" <<'PY'
import json, pathlib, sys
out, operation, target = pathlib.Path(sys.argv[1]), sys.argv[2], int(sys.argv[3])
rows = []
for path in sorted(out.glob("shard-*.json")):
    data = json.load(open(path, encoding="utf-8"))
    metrics = data.get("metrics", {})
    latency = metrics.get("api_operation_latency", {}).get("values", {})
    success = metrics.get("api_operation_success", {}).get("values", {}).get("count", 0)
    failure = metrics.get("api_system_failures", {}).get("values", {}).get("count", 0)
    duration = metrics.get("iteration_duration", {}).get("values", {}).get("max", 0)
    rows.append({"file": path.name, "success": success, "failure": failure,
                 "p95_ms": latency.get("p(95)"), "p99_ms": latency.get("p(99)"),
                 "duration_ms": duration})
wall_ms = max((row["duration_ms"] or 0 for row in rows), default=0)
success = sum(row["success"] for row in rows)
failure = sum(row["failure"] for row in rows)
summary = {
    "operationId": operation,
    "target_qps": target,
    "actual_qps": success / (wall_ms / 1000) if wall_ms else None,
    "success": success,
    "failure": failure,
    # Max is a conservative cluster percentile. Raw per-instance values remain
    # in shard files for report and skew diagnosis.
    "p95_ms_conservative": max((r["p95_ms"] for r in rows if r["p95_ms"] is not None), default=None),
    "p99_ms_conservative": max((r["p99_ms"] for r in rows if r["p99_ms"] is not None), default=None),
    "shards": rows,
}
json.dump(summary, open(out / "cluster-summary.json", "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY

echo "集群结果: $OUT/cluster-summary.json"
exit "$status"
