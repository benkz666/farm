#!/bin/sh
# 在固定的 Gateway 总资源池上执行一个总连接数档位。该脚本运行在发压 Pod 内，
# 逐 Pod 采样后由汇总器求和；结果不把“每个实例多少连接”当作容量输入。
set -u

level="${1:?total connection level is required}"
result_dir="${RESULT_DIR:-/results/gateway-connection-resource-v2/level-${level}}"
fixture="${FIXTURE:-/fixtures/gateway-connection-120000.json}"
duration="${DURATION:-120s}"
warmup_concurrency="${WARMUP_CONCURRENCY:-512}"
connections="${SHARD_CONNECTIONS:-$level}"
account_offset="${ACCOUNT_OFFSET:-0}"
shard_index="${SHARD_INDEX:-0}"
sample_enabled="${SAMPLE_METRICS:-1}"
gateway_urls="${GATEWAY_URLS:?comma-separated direct Gateway WebSocket URLs are required}"
gateway_metrics="${GATEWAY_METRICS:?comma-separated name|metrics-url entries are required}"
bench_bin="${BENCH_BIN:-/usr/local/bin/servicebench}"
ping_qps=$(( (connections + 29) / 30 ))

mkdir -p "$result_dir"
samples="$result_dir/samples.csv"
result="$result_dir/client-shard-${shard_index}.json"
log="$result_dir/client-shard-${shard_index}.log"
meta="$result_dir/meta-shard-${shard_index}.txt"

metric_value() {
  metric_name="$1"
  metric_text="$2"
  printf '%s\n' "$metric_text" | awk -v name="$metric_name" \
    '$1 == name { print $2; found=1; exit } END { if (!found) print 0 }'
}

metric_sum() {
  metric_prefix="$1"
  metric_text="$2"
  printf '%s\n' "$metric_text" | awk -v prefix="$metric_prefix" \
    'index($1, prefix) == 1 { sum += $2 } END { print sum + 0 }'
}

sample_metrics() {
  while :; do
    load_memory="$(cat /sys/fs/cgroup/memory.current 2>/dev/null || printf 0)"
    load_cpu="$(awk '/^usage_usec /{print $2; exit}' /sys/fs/cgroup/cpu.stat 2>/dev/null || printf 0)"
    old_ifs="$IFS"
    IFS=','
    for target in $gateway_metrics; do
      name="${target%%|*}"
      endpoint="${target#*|}"
      metrics="$(wget -qO- "$endpoint" 2>/dev/null || true)"
      printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$(date +%s)" "$name" \
        "$(metric_value farm_ws_connections "$metrics")" \
        "$(metric_value process_resident_memory_bytes "$metrics")" \
        "$(metric_value process_cpu_seconds_total "$metrics")" \
        "$(metric_value process_open_fds "$metrics")" \
        "$(metric_value process_max_fds "$metrics")" \
        "$(metric_value go_memstats_heap_inuse_bytes "$metrics")" \
        "$(metric_value go_goroutines "$metrics")" \
        "$(metric_sum farm_ws_connection_closed_total "$metrics")" \
        "${load_memory:-0}" "${load_cpu:-0}" >> "$samples"
    done
    IFS="$old_ifs"
    sleep 2
  done
}

printf 'total_level=%s\nshard_index=%s\nshard_connections=%s\naccount_offset=%s\nping_qps=%s\nduration=%s\nfixture=%s\ngateway_urls=%s\ngateway_metrics=%s\nbench_bin=%s\nstart_unix=%s\n' \
  "$level" "$shard_index" "$connections" "$account_offset" "$ping_qps" "$duration" "$fixture" "$gateway_urls" \
  "$gateway_metrics" "$bench_bin" "$(date +%s)" > "$meta"

monitor_pid=""
if [ "$sample_enabled" = "1" ]; then
  printf '%s\n' 'unix_s,pod,ws_connections,gateway_rss_bytes,gateway_cpu_seconds,gateway_open_fds,gateway_max_fds,gateway_heap_inuse_bytes,gateway_goroutines,gateway_closed_total,loadgen_memory_bytes,loadgen_cpu_usec' > "$samples"
  sample_metrics &
  monitor_pid=$!
fi

"$bench_bin" \
  -mode gateway-ws \
  -accounts "$fixture" \
  -gateway-urls "$gateway_urls" \
  -operation ping \
  -warmup-mode session-only \
  -qps "$ping_qps" \
  -duration "$duration" \
  -concurrency "$connections" \
  -fixed-connections "$connections" \
  -fixture-account-offset "$account_offset" \
  -warmup-concurrency "$warmup_concurrency" \
  -warmup-settle 10s \
  -per-connection-qps 1 \
  -output "$result" > "$log" 2>&1
status=$?

if [ -n "$monitor_pid" ]; then
  sleep 3
  kill "$monitor_pid" 2>/dev/null || true
  wait "$monitor_pid" 2>/dev/null || true
fi
printf 'end_unix=%s\nexit_code=%s\n' "$(date +%s)" "$status" >> "$meta"

exit "$status"
