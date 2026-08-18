#!/bin/sh
# Run one single-Gateway WebSocket connection-capacity level from the load pod.
set -u

level="${1:?connection level is required}"
result_dir="${RESULT_DIR:-/results/gateway-connection-ladder-20260815}"
fixture="${FIXTURE:-/fixtures/connection-capacity-30000.json}"
duration="${DURATION:-120s}"
warmup_concurrency="${WARMUP_CONCURRENCY:-512}"
gateway_url="${GATEWAY_URL:-ws://gateway:9002/ws}"
gateway_metrics="${GATEWAY_METRICS:-http://gateway:9302/metrics}"
ping_qps=$(( (level + 29) / 30 ))

mkdir -p "$result_dir"
samples="$result_dir/connections-${level}-samples.csv"
result="$result_dir/connections-${level}.json"
log="$result_dir/connections-${level}.log"
meta="$result_dir/connections-${level}-meta.txt"

sample_metrics() {
  while :; do
    metrics="$(wget -qO- "$gateway_metrics" 2>/dev/null || true)"
    ws="$(printf '%s\n' "$metrics" | awk '/^farm_ws_connections /{print $2; exit}')"
    rss="$(printf '%s\n' "$metrics" | awk '/^process_resident_memory_bytes /{print $2; exit}')"
    cpu="$(printf '%s\n' "$metrics" | awk '/^process_cpu_seconds_total /{print $2; exit}')"
    fds="$(printf '%s\n' "$metrics" | awk '/^process_open_fds /{print $2; exit}')"
    heap="$(printf '%s\n' "$metrics" | awk '/^go_memstats_heap_inuse_bytes /{print $2; exit}')"
    closed="$(printf '%s\n' "$metrics" | awk '/^farm_ws_connection_closed_total/{sum+=$2} END{print sum+0}')"
    load_memory="$(cat /sys/fs/cgroup/memory.current 2>/dev/null || printf 0)"
    load_cpu="$(awk '/^usage_usec /{print $2; exit}' /sys/fs/cgroup/cpu.stat 2>/dev/null || printf 0)"
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
      "$(date +%s)" "${ws:-0}" "${rss:-0}" "${cpu:-0}" "${fds:-0}" \
      "${heap:-0}" "${closed:-0}" "${load_memory:-0}" "${load_cpu:-0}" >> "$samples"
    sleep 2
  done
}

printf '%s\n' 'unix_s,ws_connections,gateway_rss_bytes,gateway_cpu_seconds,gateway_open_fds,gateway_heap_inuse_bytes,gateway_closed_total,loadgen_memory_bytes,loadgen_cpu_usec' > "$samples"
printf 'level=%s\nping_qps=%s\nduration=%s\nstart_unix=%s\n' \
  "$level" "$ping_qps" "$duration" "$(date +%s)" > "$meta"

sample_metrics &
monitor_pid=$!

servicebench \
  -mode gateway-ws \
  -accounts "$fixture" \
  -gateway-urls "$gateway_url" \
  -operation ping \
  -warmup-mode session-only \
  -qps "$ping_qps" \
  -duration "$duration" \
  -concurrency "$level" \
  -fixed-connections "$level" \
  -warmup-concurrency "$warmup_concurrency" \
  -warmup-settle 10s \
  -per-connection-qps 1 \
  -output "$result" > "$log" 2>&1
status=$?

kill "$monitor_pid" 2>/dev/null || true
wait "$monitor_pid" 2>/dev/null || true
printf 'end_unix=%s\nexit_code=%s\n' "$(date +%s)" "$status" >> "$meta"

exit "$status"
