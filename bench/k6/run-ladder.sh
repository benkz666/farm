#!/usr/bin/env bash
# 阶梯加压：从 3k 之上找 SyncFarm 读路径极限。
# 任一级 SLO/掉线/失败即停止，并写出汇总。
set -u
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
OUT="$ROOT/.run/k6-results/ladder"
mkdir -p "$OUT"

LEVELS=(5000 8000 10000 15000 20000 30000 40000 50000)
SUMMARY="$OUT/summary.csv"
echo "level,status,peak_ws,peak_sync_qps,sync_p95_ms,sync_failures,drops,checks_fail,gw_rss_mb,farm_rss_mb,notes" > "$SUMMARY"

sample_once() {
  local ts metrics ws sync_total gw_pid farm_pid gw_rss farm_rss
  ts=$(date +%s)
  metrics=$(curl -s --max-time 1 http://127.0.0.1:9302/metrics 2>/dev/null || true)
  ws=$(echo "$metrics" | awk '/^farm_ws_connections /{print $2; exit}')
  sync_total=$(echo "$metrics" | awk '/farm_ws_requests_total\{cmd="204"/{s+=$2} END{print s+0}')
  gw_pid=$(pgrep -f '/.run/bin/gateway$' | head -1)
  farm_pid=$(pgrep -f '/.run/bin/farmsvr$' | head -1)
  gw_rss=0; farm_rss=0
  [[ -n "${gw_pid:-}" ]] && gw_rss=$(awk '/VmRSS/{print $2}' "/proc/$gw_pid/status" 2>/dev/null || echo 0)
  [[ -n "${farm_pid:-}" ]] && farm_rss=$(awk '/VmRSS/{print $2}' "/proc/$farm_pid/status" 2>/dev/null || echo 0)
  echo "$ts,${ws:-0},${sync_total:-0},$gw_rss,$farm_rss"
}

run_level() {
  local n="$1"
  local tag="n${n}"
  local log="$OUT/${tag}.log"
  local json="$OUT/${tag}.json"
  local samples="$OUT/${tag}_samples.csv"
  local mon_pid=""

  echo "======== LEVEL $n ========"
  echo "ts,ws,sync_total,gw_rss_kb,farm_rss_kb" > "$samples"
  (
    while true; do
      sample_once >> "$samples"
      sleep 5
    done
  ) &
  mon_pid=$!

  # 升压时间随规模略增；持有 2.5 分钟看稳态
  local ramp_up=1m
  local ramp_step=2m
  local hold=2m30s
  local down=30s
  if (( n >= 15000 )); then ramp_up=2m; ramp_step=3m; hold=3m; fi
  if (( n >= 30000 )); then ramp_up=3m; ramp_step=4m; hold=3m; down=1m; fi

  # 连接持有略长于总阶段，避免中途重连
  local conn_dur=12m
  if (( n >= 15000 )); then conn_dur=15m; fi
  if (( n >= 30000 )); then conn_dur=20m; fi

  set +e
  BASE_URL=http://127.0.0.1:9002 \
  ACTIVITY=sync \
  SYNC_INTERVAL_MS=1000 \
  TARGET_CONNECTIONS="$n" \
  CONNECTION_DURATION="$conn_dur" \
  RAMP_UP="$ramp_up" \
  RAMP_STEP="$ramp_step" \
  RAMP_HOLD="$hold" \
  RAMP_DOWN="$down" \
  k6 run --summary-export "$json" \
    bench/k6/ws_capacity.js >"$log" 2>&1
  local rc=$?

  kill "$mon_pid" 2>/dev/null || true
  wait "$mon_pid" 2>/dev/null || true
  set +e
  # 从采样算峰值
  local peak_ws peak_qps gw_mb farm_mb
  read -r peak_ws peak_qps gw_mb farm_mb < <(python3 - <<PY
import csv
rows=list(csv.DictReader(open("$samples")))
def f(r,k):
    try: return float(r[k])
    except: return 0.0
if not rows:
    print("0 0 0 0"); raise SystemExit
ws=max(f(r,"ws") for r in rows)
# qps from sync_total deltas
best=0.0
prev=None
for r in rows:
    t=f(r,"ts"); s=f(r,"sync_total")
    if prev is not None:
        dt=max(1.0, t-prev[0])
        best=max(best, (s-prev[1])/dt)
    prev=(t,s)
# rss near peak ws
peak_rows=[r for r in rows if f(r,"ws")>=ws*0.95] or rows[-5:]
gw=sum(f(r,"gw_rss_kb") for r in peak_rows)/len(peak_rows)/1024
farm=sum(f(r,"farm_rss_kb") for r in peak_rows)/len(peak_rows)/1024
print(f"{int(ws)} {best:.1f} {gw:.0f} {farm:.0f}")
PY
)

  local sync_p95 sync_fail drops checks_fail notes status
  read -r sync_p95 sync_fail drops checks_fail < <(python3 - <<PY
import json,re
from pathlib import Path
log=Path("$log").read_text(errors="ignore")
# parse k6 end summary lines
p95="na"; fails="na"; drops="na"; cfail="na"
m=re.search(r"ws_sync_latency\.+:.*?p\(95\)=([0-9.]+)ms", log)
# k6 table format: ws_sync_latency....: avg=... p(95)=13ms
m=re.search(r"ws_sync_latency\.+:[^\n]*p\(95\)=([0-9.]+)m?s", log)
if m: p95=m.group(1)
m=re.search(r"ws_sync_failures\.+:\s*([0-9]+)", log)
if m: fails=m.group(1)
m=re.search(r"ws_connection_drops\.+:\s*([0-9]+)", log)
if m: drops=m.group(1)
# checks fail count from ✗ checks line or rate
m=re.search(r"checks\.+:\s*([0-9.]+)%", log)
if m:
    rate=float(m.group(1))
    cfail = "0" if rate >= 99.9 else "1"
print(p95, fails, drops, cfail)
PY
)

  notes=""
  status="PASS"
  if [[ "$rc" -ne 0 ]]; then status="FAIL"; notes="k6_exit_$rc"; fi
  if [[ "$sync_fail" != "na" && "$sync_fail" != "0" ]]; then status="FAIL"; notes="${notes};sync_fail"; fi
  if [[ "$drops" != "na" && "$drops" != "0" ]]; then status="FAIL"; notes="${notes};drops"; fi
  if [[ "$checks_fail" == "1" ]]; then status="FAIL"; notes="${notes};checks"; fi
  # 延迟越界也算触顶（即使 k6 因 interrupted 未判阈值）
  if [[ "$sync_p95" != "na" ]]; then
    python3 - <<PY
import sys
p95=float("$sync_p95")
sys.exit(0 if p95 < 100 else 1)
PY
    if [[ $? -ne 0 ]]; then status="FAIL"; notes="${notes};p95>=100"; fi
  fi
  # 峰值连接明显达不到目标（<90%）也视为未稳住
  if (( peak_ws < n * 90 / 100 )); then status="FAIL"; notes="${notes};peak_ws_low"; fi

  echo "$n,$status,$peak_ws,$peak_qps,$sync_p95,$sync_fail,$drops,$checks_fail,$gw_mb,$farm_mb,$notes" | tee -a "$SUMMARY"
  echo "LEVEL $n => $status peak_ws=$peak_ws qps=$peak_qps p95=$sync_p95"

  # 等连接清零再进下一档
  for _ in $(seq 1 60); do
    cur=$(curl -s --max-time 1 http://127.0.0.1:9302/metrics 2>/dev/null | awk '/^farm_ws_connections /{print $2; exit}')
    [[ "${cur:-0}" == "0" || "${cur:-0}" == "0.0" ]] && break
    sleep 2
  done
  sleep 5

  [[ "$status" == "PASS" ]]
}

echo "ladder start at $(date -Is)" | tee "$OUT/run.log"
last_pass=3000
limit_at="none"
for n in "${LEVELS[@]}"; do
  if run_level "$n"; then
    last_pass=$n
    echo "continue after PASS $n" | tee -a "$OUT/run.log"
  else
    limit_at=$n
    echo "STOP at FAIL $n (last_pass=$last_pass)" | tee -a "$OUT/run.log"
    break
  fi
done
echo "DONE last_pass=$last_pass limit_at=$limit_at" | tee -a "$OUT/run.log"
echo "summary: $SUMMARY"
cat "$SUMMARY"
