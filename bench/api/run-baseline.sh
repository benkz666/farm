#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OPERATION=${1:-all}
PROTOCOL=${PROTOCOL:-ws}
RUN_ID=${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-${OPERATION}}
RESULT_DIR=${RESULT_DIR:-"$ROOT/.run/bench-results/$RUN_ID"}
PROMETHEUS_URL=${PROMETHEUS_URL:-http://127.0.0.1:9090}
IDLE_BASELINE_SECONDS=${IDLE_BASELINE_SECONDS:-60}

mkdir -p "$RESULT_DIR"
command -v k6 >/dev/null 2>&1 || { echo "缺少 k6，无法执行 API 基线" >&2; exit 1; }

python3 "$ROOT/bench/api/prometheus_snapshot.py" --url "$PROMETHEUS_URL" --output "$RESULT_DIR/idle-start.json"
sleep "$IDLE_BASELINE_SECONDS"
python3 "$ROOT/bench/api/prometheus_snapshot.py" --url "$PROMETHEUS_URL" --output "$RESULT_DIR/idle-end.json"
python3 "$ROOT/bench/api/prometheus_snapshot.py" --url "$PROMETHEUS_URL" --output "$RESULT_DIR/load-start.json"

PPROF_PIDS=()
if [[ "${CAPTURE_PPROF:-0}" == "1" ]] && command -v curl >/dev/null 2>&1; then
  PROFILE_SECONDS=${PROFILE_SECONDS:-30}
  for spec in gateway=http://127.0.0.1:9302 farm=http://127.0.0.1:9310 social=http://127.0.0.1:9304; do
    name=${spec%%=*}
    url=${spec#*=}
    curl -fsS "$url/debug/pprof/profile?seconds=$PROFILE_SECONDS" -o "$RESULT_DIR/$name-cpu.pprof" &
    PPROF_PIDS+=("$!")
  done
fi

SCRIPT="$ROOT/bench/k6/ws_api_baseline.js"
if [[ "$PROTOCOL" == "http" ]]; then
  SCRIPT="$ROOT/bench/k6/http_api_baseline.js"
fi

set +e
SCENARIO="$OPERATION" k6 run --summary-export "$RESULT_DIR/k6-summary.json" "$SCRIPT" 2>&1 | tee "$RESULT_DIR/k6.log"
K6_STATUS=${PIPESTATUS[0]}
set -e

python3 "$ROOT/bench/api/prometheus_snapshot.py" --url "$PROMETHEUS_URL" --output "$RESULT_DIR/load-end.json"
for pid in "${PPROF_PIDS[@]}"; do wait "$pid" || true; done
if [[ "${CAPTURE_PPROF:-0}" == "1" ]] && command -v curl >/dev/null 2>&1; then
  for spec in gateway=http://127.0.0.1:9302 farm=http://127.0.0.1:9310 social=http://127.0.0.1:9304; do
    name=${spec%%=*}
    url=${spec#*=}
    curl -fsS "$url/debug/pprof/heap" -o "$RESULT_DIR/$name-heap.pprof" || true
  done
fi

SUCCESS_COUNT=$(python3 - "$RESULT_DIR/k6-summary.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
print(int(data.get("metrics", {}).get("api_operation_success", {}).get("values", {}).get("count", 0)))
PY
)

python3 "$ROOT/bench/api/service_demand.py" \
  --idle-start "$RESULT_DIR/idle-start.json" \
  --idle-end "$RESULT_DIR/idle-end.json" \
  --load-start "$RESULT_DIR/load-start.json" \
  --load-end "$RESULT_DIR/load-end.json" \
  --successes "$SUCCESS_COUNT" \
  --operation "$OPERATION" \
  --output "$RESULT_DIR/service-demand.json"

python3 "$ROOT/bench/api/report.py" \
  --operation "$OPERATION" \
  --protocol "$PROTOCOL" \
  --summary "$RESULT_DIR/k6-summary.json" \
  --demand "$RESULT_DIR/service-demand.json" \
  --output "$RESULT_DIR/report.csv"

python3 - "$RESULT_DIR/environment.json" "$ROOT" "$OPERATION" <<'PY'
import json, os, platform, subprocess, sys, time
output, root, operation = sys.argv[1:]
try:
    commit = subprocess.check_output(["git", "-C", root, "rev-parse", "HEAD"], universal_newlines=True).strip()
except Exception:
    commit = "unknown"
data = {
    "captured_at": time.time(),
    "git_commit": commit,
    "host": platform.node(),
    "platform": platform.platform(),
    "operation": operation,
    "base_url": os.environ.get("BASE_URL", "http://127.0.0.1:9002"),
}
with open(output, "w", encoding="utf-8") as target:
    json.dump(data, target, ensure_ascii=False, indent=2, sort_keys=True)
    target.write("\n")
PY

python3 - "$RESULT_DIR/grafana-range.json" "$RESULT_DIR/load-start.json" "$RESULT_DIR/load-end.json" <<'PY'
import json, sys
start = json.load(open(sys.argv[2]))["captured_at"]
end = json.load(open(sys.argv[3]))["captured_at"]
json.dump({"from_ms": int(start * 1000), "to_ms": int(end * 1000)}, open(sys.argv[1], "w"), indent=2)
PY

echo "结果目录: $RESULT_DIR"
exit "$K6_STATUS"
