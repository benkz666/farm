#!/usr/bin/env bash
# 在单Gateway固定拓扑下运行一个垂直容量档：三发压分片、A/B/C/D窗口、逐接口SLO。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
namespace="${NAMESPACE:-benkz}"
tier="${1:?用法: run_vertical_capacity_experiment.sh <1|2|10k>}"
result_root="${RESULT_ROOT:-${repo_root}/bench/results/vertical-capacity-v1-20260816}"
remote_root="${REMOTE_RESULT_ROOT:-/results/vertical-capacity-v1-20260816}"
model_local="${repo_root}/bench/model/user-behavior.capacity-100-v1.json"
model_remote="/fixtures/user-behavior.capacity-100-v1.json"
duration_seconds="${DURATION_SECONDS:-300}"
idle_seconds="${IDLE_WINDOW_SECONDS:-60}"
state_seconds="${STATE_WINDOW_SECONDS:-60}"
recovery_seconds="${RECOVERY_WINDOW_SECONDS:-120}"
warmup_concurrency="${WARMUP_CONCURRENCY:-512}"
reset_concurrency="${RESET_CONCURRENCY:-8}"
gateway_handshake_qps="${GATEWAY_HANDSHAKE_QPS:-0}"
global_qps_override="${GLOBAL_QPS_OVERRIDE:-0}"
exclude_operations="${EXCLUDE_OPERATIONS:-}"
shard_actors_override="${SHARD_ACTORS_OVERRIDE:-0}"

case "$tier" in
  1)
    global_qps=3000 shard_qps_values=(1000 1000 1000) shard_connections=12000 shard_actors=16260
    fixture_prefix=/fixtures/vertical-unit-1x-shard
    run_id="unit-1x-qps-3000"
    ;;
  2)
    global_qps=6000 shard_qps_values=(2000 2000 2000) shard_connections=24000 shard_actors=32520
    fixture_prefix=/fixtures/vertical-unit-2x-shard
    run_id="unit-2x-qps-6000"
    ;;
  10k)
    global_qps=10000 shard_qps_values=(3334 3333 3333) shard_connections=40000 shard_actors=54200
    fixture_prefix=/fixtures/vertical-unit-10k-shard
    run_id="unit-10k-qps-10000"
    ;;
  *)
    printf 'tier必须是1、2或10k\n' >&2
    exit 2
    ;;
esac
fixture_accounts=$((shard_actors + 1))
if ((global_qps_override > 0)); then
  global_qps="$global_qps_override"
  shard_qps_values=(
    $(((global_qps + 2) / 3))
    $(((global_qps + 1) / 3))
    $((global_qps / 3))
  )
  run_id="unit-10k-state-qps-${global_qps}"
fi
if ((shard_actors_override > 0)); then
  if ((shard_actors_override < shard_connections)); then
    printf 'SHARD_ACTORS_OVERRIDE不能小于每分片连接数%s\n' "$shard_connections" >&2
    exit 2
  fi
  shard_actors="$shard_actors_override"
fi

run_dir="${result_root}/${run_id}"
remote_dir="${remote_root}/${run_id}"
mkdir -p "$run_dir"
exec > >(tee -a "${run_dir}/run.log") 2>&1

timestamp_ms() {
  date +%s%3N
}

wait_journal_idle() {
  local timeout_seconds="$1"
  local deadline=$((SECONDS + timeout_seconds))
  local values pending=1 lag=1
  while ((SECONDS < deadline)); do
    values="$({
      kubectl -n "$namespace" exec redis-0 -- sh -lc '
        for key in $(redis-cli --scan --pattern "*:events"); do
          redis-cli --raw XINFO GROUPS "$key"
        done | awk '\''
          $0 == "pending" { getline; pending += $0 }
          $0 == "lag" { getline; lag += $0 }
          END { print pending + 0, lag + 0 }
        '\''
      '
    } 2>/dev/null)"
    read -r pending lag <<<"$values"
    if [[ "${pending:-1}" == 0 && "${lag:-1}" == 0 ]]; then
      JOURNAL_IDLE_AT_MS="$(timestamp_ms)"
      return 0
    fi
    sleep 2
  done
  printf 'Journal未在%ss内排空：pending=%s lag=%s\n' "$timeout_seconds" "$pending" "$lag" >&2
  return 1
}

printf '配置%s容量资源档...\n' "$tier"
"${repo_root}/bench/api/configure_vertical_capacity_profile.sh" "$tier"

mapfile -t k6_pods < <(
  kubectl -n "$namespace" get pod -l app.kubernetes.io/name=k6 \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort
)
if [[ "${#k6_pods[@]}" != 3 ]]; then
  printf '需要3个发压Pod，实际得到%s个\n' "${#k6_pods[@]}" >&2
  exit 1
fi
kubectl -n "$namespace" cp "$model_local" "${k6_pods[0]}:${model_remote}"
kubectl -n "$namespace" exec "${k6_pods[0]}" -- mkdir -p "$remote_dir"

wait_journal_idle 300
if [[ "${SKIP_FIXTURE_RESET:-0}" != 1 ]]; then
  mysql_dsn="$(kubectl -n "$namespace" get secret farm-secrets -o jsonpath='{.data.mysql-dsn}' | base64 -d)"
  printf '并行重置3份mixed合法夹具，每份%s个账号...\n' "$fixture_accounts"
  reset_pids=()
  for shard in 0 1 2; do
    fixture="${fixture_prefix}-${shard}.json"
    kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- benchfixture \
      -mysql-dsn "$mysql_dsn" \
      -redis-addr redis:6379 \
      -concurrency "$reset_concurrency" \
      -profile mixed \
      -time-profile authentic \
      -reset-input "$fixture" \
      >"${run_dir}/reset-shard-${shard}.log" 2>&1 &
    reset_pids+=("$!")
  done
  reset_status=0
  for pid in "${reset_pids[@]}"; do
    wait "$pid" || reset_status=1
  done
  unset mysql_dsn
  if [[ "$reset_status" != 0 ]]; then
    printf '夹具重置失败，见reset-shard日志\n' >&2
    exit 1
  fi
else
  printf '复用上一轮未进入C窗口的合法夹具，跳过重复重置\n'
fi

for shard in 0 1 2; do
  fixture="${fixture_prefix}-${shard}.json"
  metadata_count="$(kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- grep -c '"task_ids"' "$fixture")"
  if [[ "$metadata_count" != "$fixture_accounts" ]]; then
    printf '分片%s元数据数量=%s，期望=%s\n' "$shard" "$metadata_count" "$fixture_accounts" >&2
    exit 1
  fi
done

# 夹具直接写数据库；重启应用清除Actor和本地缓存，数据服务保持当前档资源。
kubectl -n "$namespace" rollout restart deployment/farm deployment/gateway deployment/social
for workload in deployment/farm deployment/gateway deployment/social; do
  kubectl -n "$namespace" rollout status "$workload" --timeout=10m
done
# One Gateway process is exposed through both Service IP and Pod IP. This only
# expands the client-side TCP four-tuple space and does not add a Gateway replica.
gateway_pod_ip="$(
  kubectl -n "$namespace" get pod -l app.kubernetes.io/name=gateway \
    -o jsonpath='{.items[0].status.podIP}'
)"
if [[ -z "$gateway_pod_ip" ]]; then
  printf '无法获取单 Gateway Pod IP\n' >&2
  exit 1
fi
gateway_urls="ws://gateway:9002/ws,ws://${gateway_pod_ip}:9002/ws"
wait_journal_idle 300
sleep 30

kubectl -n "$namespace" get pods -o json >"${run_dir}/pods-before.json"
kubectl -n "$namespace" get deployment gateway farm social k6 -o json >"${run_dir}/deployments.json"
kubectl -n "$namespace" get statefulset mysql redis -o json >"${run_dir}/statefulsets.json"

idle_start_ms="$(timestamp_ms)"
printf 'A窗口：空载%ss...\n' "$idle_seconds"
sleep "$idle_seconds"
idle_end_ms="$(timestamp_ms)"

measurement_start_file="${remote_dir}/measurement-start"
kubectl -n "$namespace" exec "${k6_pods[0]}" -- sh -lc \
  "rm -f '${measurement_start_file}' '${remote_dir}'/ready-shard-*"
printf '启动3个发压分片：QPS=%s/%s/%s，每分片%s连接、%s Actor；全部就绪后统一释放C窗口\n' \
  "${shard_qps_values[0]}" "${shard_qps_values[1]}" "${shard_qps_values[2]}" \
  "$shard_connections" "$shard_actors"
load_pids=()
for shard in 0 1 2; do
  shard_qps="${shard_qps_values[$shard]}"
  fixture="${fixture_prefix}-${shard}.json"
  remote_result="${remote_dir}/client-shard-${shard}.json"
  kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- servicebench \
    -mode gateway-mixed \
    -accounts "$fixture" \
    -behavior-model "$model_remote" \
    -exclude-operations "$exclude_operations" \
    -gateway-urls "$gateway_urls" \
    -qps "$shard_qps" \
    -duration "${duration_seconds}s" \
    -concurrency "$shard_connections" \
    -fixed-connections "$shard_connections" \
    -resident-actors "$shard_actors" \
    -resident-actor-refresh 0s \
    -warmup-concurrency "$warmup_concurrency" \
    -warmup-settle "${state_seconds}s" \
    -measurement-ready-file "${remote_dir}/ready-shard-${shard}" \
    -measurement-start-file "$measurement_start_file" \
    -output "$remote_result" \
    >"${run_dir}/client-shard-${shard}.log" 2>&1 &
  load_pids+=("$!")
done

ready_deadline=$((SECONDS + 300))
while true; do
  ready_count=0
  for shard in 0 1 2; do
    if kubectl -n "$namespace" exec "${k6_pods[0]}" -- \
      test -f "${remote_dir}/ready-shard-${shard}" >/dev/null 2>&1; then
      ready_count=$((ready_count + 1))
    fi
  done
  if [[ "$ready_count" == 3 ]]; then
    break
  fi
  for pid in "${load_pids[@]}"; do
    if ! kill -0 "$pid" 2>/dev/null; then
      printf '发压分片在就绪前退出，见client-shard日志\n' >&2
      exit 1
    fi
  done
  if ((SECONDS >= ready_deadline)); then
    printf '等待发压分片就绪超时：%s/3\n' "$ready_count" >&2
    exit 1
  fi
  sleep 1
done
measurement_start_ms=$(( $(timestamp_ms) + 15000 ))
kubectl -n "$namespace" exec "${k6_pods[0]}" -- sh -lc \
  "printf '%s\\n' '${measurement_start_ms}' > '${measurement_start_file}'"
printf '三个分片已就绪，统一C窗口开始=%s\n' "$measurement_start_ms"

handshake_pids=()
if ((gateway_handshake_qps > 0)); then
  handshake_base=$((gateway_handshake_qps / 3))
  handshake_remainder=$((gateway_handshake_qps % 3))
  printf '叠加独立账号会话流量：总QPS=%s，账号偏移=%s\n' \
    "$gateway_handshake_qps" "$shard_connections"
  for shard in 0 1 2; do
    shard_handshake_qps="$handshake_base"
    if ((shard < handshake_remainder)); then
      shard_handshake_qps=$((shard_handshake_qps + 1))
    fi
    fixture="${fixture_prefix}-${shard}.json"
    remote_handshake_result="${remote_dir}/handshake-shard-${shard}.json"
    kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- servicebench \
      -mode gateway-handshake \
      -accounts "$fixture" \
      -fixture-account-offset "$shard_connections" \
      -gateway-urls "$gateway_urls" \
      -qps "$shard_handshake_qps" \
      -duration "${duration_seconds}s" \
      -concurrency 128 \
      -measurement-start-unix-ms "$measurement_start_ms" \
      -output "$remote_handshake_result" \
      >"${run_dir}/handshake-shard-${shard}.log" 2>&1 &
    handshake_pids+=("$!")
  done
fi

load_status=0
for pid in "${load_pids[@]}"; do
  wait "$pid" || load_status=1
done
for pid in "${handshake_pids[@]}"; do
  wait "$pid" || load_status=1
done
if [[ "$load_status" != 0 ]]; then
  printf '发压分片失败，见client-shard日志\n' >&2
  exit 1
fi

for shard in 0 1 2; do
  kubectl -n "$namespace" cp \
    "${k6_pods[0]}:${remote_dir}/client-shard-${shard}.json" \
    "${run_dir}/client-shard-${shard}.json"
  if ((gateway_handshake_qps > 0)); then
    kubectl -n "$namespace" cp \
      "${k6_pods[0]}:${remote_dir}/handshake-shard-${shard}.json" \
      "${run_dir}/handshake-shard-${shard}.json"
  fi
done

python3 "${repo_root}/bench/api/merge_capacity_shards.py" \
  --result "${run_dir}/client-shard-0.json" \
  --result "${run_dir}/client-shard-1.json" \
  --result "${run_dir}/client-shard-2.json" \
  --output "${run_dir}/client-merged.json"

c_end_ms=$((measurement_start_ms + duration_seconds * 1000))
drain_check_started_ms="$(timestamp_ms)"
wait_journal_idle "$recovery_seconds"
journal_idle_ms="$JOURNAL_IDLE_AT_MS"
recovery_end_ms=$((c_end_ms + recovery_seconds * 1000))
while (( $(timestamp_ms) < recovery_end_ms )); do
  sleep 1
done
sleep 16

python3 "${repo_root}/bench/api/write_capacity_window_context.py" \
  --output "${run_dir}/window-context.json" \
  --run-id "$run_id" \
  --idle-start-ms "$idle_start_ms" \
  --idle-end-ms "$idle_end_ms" \
  --drain-check-start-ms "$drain_check_started_ms" \
  --journal-idle-ms "$journal_idle_ms" \
  --recovery-end-ms "$recovery_end_ms" \
  --recovery-seconds "$recovery_seconds"

node_ip="$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
prom_url="${PROM_URL:-http://${node_ip}:30909}"
python3 "${repo_root}/bench/api/prometheus_capacity_windows.py" \
  --url "$prom_url" \
  --context "${run_dir}/window-context.json" \
  --result "${run_dir}/client-merged.json" \
  --output "${run_dir}/prometheus-windows.json"

python3 "${repo_root}/bench/api/capacity_slo.py" \
  --model "$model_local" \
  --result "${run_dir}/client-shard-0.json" \
  --result "${run_dir}/client-shard-1.json" \
  --result "${run_dir}/client-shard-2.json" \
  --sharded \
  --exclude-operations "$exclude_operations" \
  --output "${run_dir}/slo.json"

kubectl -n "$namespace" get pods -o json >"${run_dir}/pods-after.json"
printf '完成：%s\n' "$run_dir"
rg '"target_qps"|"verdict"|"resident_actor_refresh_failures"' \
  "${run_dir}/client-merged.json" "${run_dir}/slo.json" | tail -12 || true
