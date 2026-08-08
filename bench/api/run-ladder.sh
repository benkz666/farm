#!/usr/bin/env bash
# 单接口阶梯：100 QPS（认证 10 QPS）起步，翻倍至多 8 档；失败后补测中点。
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OPERATION=${1:?用法: bench/api/run-ladder.sh <operationId>}
PROTOCOL=${PROTOCOL:-ws}
if [[ -z "${START_QPS+x}" ]]; then
  if [[ "$OPERATION" == "register" || "$OPERATION" == "login" ]]; then START_QPS=10; else START_QPS=100; fi
fi
LEVELS=${LEVELS:-8}
WARMUP=${WARMUP:-1m}
STABLE=${STABLE:-3m}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-ladder-${OPERATION}}
OUT=${RESULT_DIR:-"$ROOT/.run/bench-results/$RUN_ID"}
mkdir -p "$OUT"

SCRIPT="$ROOT/bench/k6/ws_api_baseline.js"
if [[ "$PROTOCOL" == "http" ]]; then SCRIPT="$ROOT/bench/k6/http_api_baseline.js"; fi

run_once() {
  local qps=$1 phase=$2 repeat=$3
  local vus=$(( (qps + 7) / 8 ))
  if [[ "$PROTOCOL" == "http" ]]; then vus=$(( (qps + 3) / 4 )); fi
  local duration=$STABLE
  [[ "$phase" == "warmup" ]] && duration=$WARMUP
  local file="$OUT/qps-${qps}-${phase}-${repeat}.json"
  echo "[$phase] operation=$OPERATION target_qps=$qps vus=$vus duration=$duration"
  SCENARIO="$OPERATION" MODE=load TARGET_QPS="$qps" TARGET_VUS="$vus" DURATION="$duration" \
    k6 run --summary-export "$file" "$SCRIPT"
}

last_pass=0
first_fail=0
qps=$START_QPS
for ((level=1; level<=LEVELS; level++)); do
  run_once "$qps" warmup 1 || true
  if run_once "$qps" stable 1; then
    last_pass=$qps
    qps=$((qps * 2))
  else
    first_fail=$qps
    break
  fi
done

if (( first_fail > 0 && last_pass > 0 && first_fail - last_pass > 1 )); then
  midpoint=$(( (last_pass + first_fail) / 2 ))
  run_once "$midpoint" warmup 1 || true
  if run_once "$midpoint" stable 1; then last_pass=$midpoint; else first_fail=$midpoint; fi
fi

# 边界档总计三次，报告时取三份结果的中位数。
if (( last_pass > 0 )); then
  run_once "$last_pass" stable 2
  run_once "$last_pass" stable 3
fi

python3 - "$OUT" "$OPERATION" "$last_pass" "$first_fail" <<'PY'
import json, pathlib, statistics, sys
root, operation, last_pass, first_fail = pathlib.Path(sys.argv[1]), sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
rows = []
for path in sorted(root.glob("qps-*-stable-*.json")):
    data = json.load(open(path))
    metrics = data.get("metrics", {})
    latency = metrics.get("api_operation_latency", {}).get("values", {})
    failure = metrics.get("api_system_failure_rate", {}).get("values", {})
    rows.append({"file": path.name, "avg_ms": latency.get("avg"), "p95_ms": latency.get("p(95)"),
                 "p99_ms": latency.get("p(99)"), "error_rate": failure.get("rate")})
boundary = [r for r in rows if r["file"].startswith("qps-%d-stable-" % last_pass)]
def median(name):
    values = [row[name] for row in boundary if row[name] is not None]
    return statistics.median(values) if values else None
result = {"operationId": operation, "max_stable_qps": last_pass or None,
          "first_failed_qps": first_fail or None, "boundary_median": {
              "avg_ms": median("avg_ms"), "p95_ms": median("p95_ms"),
              "p99_ms": median("p99_ms"), "error_rate": median("error_rate")}, "runs": rows}
json.dump(result, open(root / "ladder-summary.json", "w"), ensure_ascii=False, indent=2)
PY

echo "阶梯结果: $OUT/ladder-summary.json"
