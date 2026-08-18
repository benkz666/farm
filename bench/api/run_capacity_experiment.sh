#!/usr/bin/env bash
# 对一个QPS档执行可复现的A/B/C/D容量实验。脚本在每轮前重置合法夹具并
# 清空进程内Actor/缓存，保留全部原始客户端结果、Kubernetes快照和Prometheus窗口。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
namespace="${NAMESPACE:-benkz}"
run_id="${1:?用法: run_capacity_experiment.sh <run-id> <target-qps> [duration-seconds]}"
target_qps="${2:?缺少target-qps}"
duration_seconds="${3:-30}"

model_local="${MODEL_LOCAL:-${repo_root}/bench/model/user-behavior.capacity-100-v1.json}"
model_remote="${MODEL_REMOTE:-/fixtures/user-behavior.capacity-100-v1.json}"
fixture_remote="${FIXTURE_REMOTE:-/fixtures/capacity-100-v1-15000x18.json}"
result_root="${RESULT_ROOT:-${repo_root}/bench/results/capacity-100-v1-20260815}"
run_dir="${result_root}/${run_id}"
remote_dir="/results/capacity-100-v1-20260815/${run_id}"
result_local="${run_dir}/client.json"
result_remote="${remote_dir}/client.json"
window_context="${run_dir}/window-context.json"
window_result="${run_dir}/prometheus-windows.json"
slo_result="${run_dir}/slo.json"

connections="${FIXED_CONNECTIONS:-15000}"
expected_gateways="${EXPECTED_GATEWAYS:-3}"
warmup_concurrency="${WARMUP_CONCURRENCY:-256}"
settle_seconds="${POST_RESTART_SETTLE_SECONDS:-30}"
idle_seconds="${IDLE_WINDOW_SECONDS:-60}"
state_seconds="${STATE_WINDOW_SECONDS:-45}"
recovery_seconds="${RECOVERY_WINDOW_SECONDS:-60}"
reset_concurrency="${RESET_CONCURRENCY:-8}"
certify="${CERTIFY:-0}"
skip_slo="${SKIP_SLO:-0}"

if ! [[ "$target_qps" =~ ^[1-9][0-9]*$ && "$duration_seconds" =~ ^[1-9][0-9]*$ ]]; then
  printf 'target-qps和duration必须是正整数\n' >&2
  exit 2
fi
mkdir -p "$run_dir"
exec > >(tee -a "${run_dir}/run.log") 2>&1

timestamp_ms() {
  date +%s%3N
}

wait_journal_idle() {
  local timeout_seconds="${1:-180}"
  local deadline=$((SECONDS + timeout_seconds))
  local values pending lag
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
    if [[ "${pending:-1}" == "0" && "${lag:-1}" == "0" ]]; then
      JOURNAL_IDLE_AT_MS="$(timestamp_ms)"
      return 0
    fi
    sleep 1
  done
  printf 'Journal未在%s秒内排空：pending=%s lag=%s\n' \
    "$timeout_seconds" "${pending:-unknown}" "${lag:-unknown}" >&2
  return 1
}

pod_for() {
  kubectl -n "$namespace" get pod \
    -l "app.kubernetes.io/name=$1" \
    -o jsonpath='{.items[0].metadata.name}'
}

printf '实验 %s：目标=%s QPS，C窗口=%ss，连接=%s\n' \
  "$run_id" "$target_qps" "$duration_seconds" "$connections"
k6_pod="$(pod_for k6)"

# 复制模型而不是依赖镜像内旧副本，保证P90 SLO和接口权重与工作区文档一致。
kubectl -n "$namespace" cp "$model_local" "$k6_pod:$model_remote"
kubectl -n "$namespace" exec "$k6_pod" -- mkdir -p "$remote_dir"

wait_journal_idle 300
mysql_dsn="$(kubectl -n "$namespace" get secret farm-secrets \
  -o jsonpath='{.data.mysql-dsn}' | base64 -d)"
printf '重置15,000账号的mixed合法状态（并发=%s）...\n' "$reset_concurrency"
kubectl -n "$namespace" exec "$k6_pod" -- benchfixture \
  -mysql-dsn "$mysql_dsn" \
  -redis-addr redis:6379 \
  -concurrency "$reset_concurrency" \
  -profile mixed \
  -time-profile authentic \
  -reset-input "$fixture_remote"
unset mysql_dsn

task_metadata_count="$(kubectl -n "$namespace" exec "$k6_pod" -- \
  grep -c '"task_ids"' "$fixture_remote")"
mail_metadata_count="$(kubectl -n "$namespace" exec "$k6_pod" -- \
  grep -c '"mail_claim_id"' "$fixture_remote")"
if [[ "$task_metadata_count" != "$connections" || "$mail_metadata_count" != "$connections" ]]; then
  printf 'mixed夹具元数据不完整：task_ids=%s mail_claim_id=%s want=%s\n' \
    "$task_metadata_count" "$mail_metadata_count" "$connections" >&2
  exit 1
fi
printf 'mixed夹具元数据校验通过：task_ids/mail_claim_id均为%s条\n' "$connections"

# 夹具直接改MySQL，必须重启Farm清除旧Actor；Gateway/Social也重启以统一本地缓存。
kubectl -n "$namespace" rollout restart deployment/farm deployment/gateway deployment/social
for workload in deployment/farm deployment/gateway deployment/social; do
  kubectl -n "$namespace" rollout status "$workload" --timeout=8m
done
wait_journal_idle 300
printf '服务启动后稳定等待%ss...\n' "$settle_seconds"
sleep "$settle_seconds"

kubectl -n "$namespace" get pods -o json >"${run_dir}/pods-before.json"
kubectl -n "$namespace" get deployment gateway farm social k6 -o json \
  >"${run_dir}/deployments-before.json"
kubectl -n "$namespace" get statefulset mysql redis -o json \
  >"${run_dir}/statefulsets-before.json"

idle_start_ms="$(timestamp_ms)"
printf '窗口A：空闲基线%ss...\n' "$idle_seconds"
sleep "$idle_seconds"
idle_end_ms="$(timestamp_ms)"

mapfile -t gateway_ips < <(
  kubectl -n "$namespace" get pod -l app.kubernetes.io/name=gateway \
    -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' | sort
)
if [[ "${#gateway_ips[@]}" != "$expected_gateways" ]]; then
  printf '需要%s个Gateway Pod，实际得到%s个\n' "$expected_gateways" "${#gateway_ips[@]}" >&2
  exit 1
fi
gateway_urls=""
for ip in "${gateway_ips[@]}"; do
  [[ -n "$gateway_urls" ]] && gateway_urls+=","
  gateway_urls+="ws://${ip}:9002/ws"
done

printf '窗口B：15,000连接/Actor预热后静置%ss；随后窗口C发压...\n' "$state_seconds"
kubectl -n "$namespace" exec "$k6_pod" -- servicebench \
  -mode gateway-mixed \
  -accounts "$fixture_remote" \
  -behavior-model "$model_remote" \
  -gateway-urls "$gateway_urls" \
  -qps "$target_qps" \
  -duration "${duration_seconds}s" \
  -concurrency "$connections" \
  -fixed-connections "$connections" \
  -warmup-concurrency "$warmup_concurrency" \
  -warmup-settle "${state_seconds}s" \
  -output "$result_remote" >/dev/null

kubectl -n "$namespace" cp "$k6_pod:$result_remote" "$result_local"
measurement_start_ms="$(sed -n 's/.*"measurement_start_unix_ms": \([0-9]*\).*/\1/p' "$result_local")"
measurement_millis="$(sed -n 's/.*"measurement_millis": \([0-9]*\).*/\1/p' "$result_local")"
if [[ -z "$measurement_start_ms" || -z "$measurement_millis" ]]; then
  printf '无法从servicebench结果解析测量窗口\n' >&2
  exit 1
fi
c_end_ms=$((measurement_start_ms + measurement_millis))

drain_check_started_ms="$(timestamp_ms)"
wait_journal_idle "$recovery_seconds"
journal_idle_ms="$JOURNAL_IDLE_AT_MS"
now_ms="$(timestamp_ms)"
recovery_end_ms=$((c_end_ms + recovery_seconds * 1000))
while ((now_ms < recovery_end_ms)); do
  sleep 1
  now_ms="$(timestamp_ms)"
done
# 等下一次15秒抓取落盘，再查询截止到recovery_end_ms的历史窗口。
sleep 16

python3 "${repo_root}/bench/api/write_capacity_window_context.py" \
  --output "$window_context" \
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
  --context "$window_context" \
  --result "$result_local" \
  --output "$window_result"

if [[ "$skip_slo" != "1" ]]; then
  slo_args=()
  if [[ "$certify" != "1" ]]; then
    slo_args+=(--screening)
  fi
  python3 "${repo_root}/bench/api/capacity_slo.py" \
    --model "$model_local" \
    --result "$result_local" \
    --output "$slo_result" \
    "${slo_args[@]}"
fi

kubectl -n "$namespace" get pods -o json >"${run_dir}/pods-after.json"
printf '完成：%s\n' "$run_dir"
summary_files=("$result_local")
[[ -f "$slo_result" ]] && summary_files+=("$slo_result")
rg '"target_qps"|"succeeded"|"failed"|"p90_ms"|"p99_ms"|"verdict"' \
  "${summary_files[@]}" | tail -12 || true
