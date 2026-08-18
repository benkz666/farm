#!/usr/bin/env bash
# Social 模块隔离实验：真实 Social/MySQL/Redis，直接覆盖四类客户端操作和派生关系查询。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
namespace="${NAMESPACE:-benkz}"
result_root="${RESULT_ROOT:-${repo_root}/bench/results/module-isolation-20260817/social}"
remote_root="${REMOTE_RESULT_ROOT:-/results/module-isolation-20260817/social}"
run_id="unit-10k-social-qps-3210"
run_dir="${result_root}/${run_id}"
remote_dir="${remote_root}/${run_id}"
model_local="${repo_root}/bench/model/user-behavior.capacity-100-v1.json"
model_remote="/fixtures/user-behavior.capacity-100-v1.json"
fixture_remote="/fixtures/vertical-unit-10k-shard-0.json"
qps="${SOCIAL_QPS:-3210}"
duration_seconds="${DURATION_SECONDS:-60}"
idle_seconds="${IDLE_WINDOW_SECONDS:-15}"
recovery_seconds="${RECOVERY_WINDOW_SECONDS:-15}"

mkdir -p "$run_dir"
exec > >(tee -a "${run_dir}/run.log") 2>&1

timestamp_ms() { date +%s%3N; }

kubectl -n "$namespace" set resources deployment/social \
  --requests=cpu=2,memory=2Gi --limits=cpu=2,memory=2Gi
kubectl -n "$namespace" set env deployment/social GOMAXPROCS=2 GOMEMLIMIT=1600MiB
kubectl -n "$namespace" rollout status deployment/social --timeout=10m

k6_pod="$(kubectl -n "$namespace" get pod -l app.kubernetes.io/name=k6 \
  -o jsonpath='{.items[0].metadata.name}')"
kubectl -n "$namespace" cp "$model_local" "${k6_pod}:${model_remote}"
kubectl -n "$namespace" exec "$k6_pod" -- mkdir -p "$remote_dir"

# 重启清除本地缓存；MySQL/Redis数据保留，直接测真实依赖。
kubectl -n "$namespace" rollout restart deployment/social
kubectl -n "$namespace" rollout status deployment/social --timeout=10m
sleep 30

kubectl -n "$namespace" get pods -o json >"${run_dir}/pods-before.json"
kubectl -n "$namespace" get deployment social k6 -o json >"${run_dir}/deployments.json"
kubectl -n "$namespace" get statefulset mysql redis -o json >"${run_dir}/statefulsets.json"

idle_start_ms="$(timestamp_ms)"
sleep "$idle_seconds"
idle_end_ms="$(timestamp_ms)"

# 留出统一的预热窗口；预热结束时刻由结果中的 state_ready_unix_ms 记录。
measurement_start_ms=$(( $(timestamp_ms) + 30000 ))
kubectl -n "$namespace" exec "$k6_pod" -- servicebench \
  -mode social-mixed \
  -target social:9204 \
  -accounts "$fixture_remote" \
  -behavior-model "$model_remote" \
  -qps "$qps" \
  -duration "${duration_seconds}s" \
  -concurrency 512 \
  -social-hot-users 2600 \
  -measurement-start-unix-ms "$measurement_start_ms" \
  -output "${remote_dir}/client.json"

kubectl -n "$namespace" cp "${k6_pod}:${remote_dir}/client.json" "${run_dir}/client.json"
cp "${run_dir}/client.json" "${run_dir}/client-merged.json"

read -r c_end_ms state_ready_ms < <(
  python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["measurement_end_unix_ms"], d["state_ready_unix_ms"])' \
    "${run_dir}/client.json"
)
recovery_end_ms=$((c_end_ms + recovery_seconds * 1000))
while (( $(timestamp_ms) < recovery_end_ms )); do sleep 1; done
sleep 16

python3 "${repo_root}/bench/api/write_capacity_window_context.py" \
  --output "${run_dir}/window-context.json" \
  --run-id "$run_id" \
  --idle-start-ms "$idle_start_ms" \
  --idle-end-ms "$idle_end_ms" \
  --drain-check-start-ms "$c_end_ms" \
  --journal-idle-ms "$c_end_ms" \
  --recovery-end-ms "$recovery_end_ms" \
  --recovery-seconds "$recovery_seconds"

node_ip="$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
prom_url="${PROM_URL:-http://${node_ip}:30909}"
python3 "${repo_root}/bench/api/prometheus_capacity_windows.py" \
  --url "$prom_url" \
  --context "${run_dir}/window-context.json" \
  --result "${run_dir}/client.json" \
  --output "${run_dir}/prometheus-windows.json"

python3 "${repo_root}/bench/api/capacity_slo.py" \
  --model "$model_local" \
  --result "${run_dir}/client.json" \
  --extra-operation are-friends \
  --only-operations 'friend-list,gen-share,search-user,list-friend-requests,are-friends' \
  --output "${run_dir}/slo.json"

kubectl -n "$namespace" get pods -o json >"${run_dir}/pods-after.json"
printf '完成：%s\n' "$run_dir"
grep -E '"target_qps"|"verdict"' "${run_dir}/slo.json" | tail -2
