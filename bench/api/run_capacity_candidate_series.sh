#!/usr/bin/env bash
# 候选最高档的统计认证：资源窗口只在完整A/B/C/D轮采一次；本脚本重复
# “合法夹具重置 -> 应用层清缓存 -> 15,000连接/Actor -> 31接口混合负载”，
# 直到最低权重接口也累计到足够样本。每轮仍独立执行SLO判定并等待Journal排空。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
namespace="${NAMESPACE:-benkz}"
series_id="${1:?用法: run_capacity_candidate_series.sh <series-id> <qps> <rounds>}"
target_qps="${2:?缺少qps}"
rounds="${3:?缺少新增轮数}"

model_local="${MODEL_LOCAL:-${repo_root}/bench/model/user-behavior.capacity-100-v1.json}"
model_remote="${MODEL_REMOTE:-/fixtures/user-behavior.capacity-100-v1.json}"
fixture_remote="${FIXTURE_REMOTE:-/fixtures/capacity-100-v1-15000x18.json}"
result_root="${RESULT_ROOT:-${repo_root}/bench/results/capacity-100-v1-20260815}"
series_dir="${result_root}/${series_id}"
remote_dir="/results/capacity-100-v1-20260815/${series_id}"
base_result="${CERT_BASE_RESULT:-${result_root}/opt-projectors12-16000-r1/client.json}"
connections="${FIXED_CONNECTIONS:-15000}"
reset_concurrency="${RESET_CONCURRENCY:-8}"
settle_seconds="${CERT_SETTLE_SECONDS:-10}"
state_seconds="${CERT_STATE_SECONDS:-45}"
duration_seconds="${CERT_DURATION_SECONDS:-30}"

mkdir -p "$series_dir"
exec > >(tee -a "${series_dir}/series.log") 2>&1

if ! [[ "$target_qps" =~ ^[1-9][0-9]*$ && "$rounds" =~ ^[1-9][0-9]*$ ]]; then
  printf 'qps和rounds必须是正整数\n' >&2
  exit 2
fi
if [[ ! -f "$base_result" ]]; then
  printf '缺少首轮候选结果：%s\n' "$base_result" >&2
  exit 2
fi

timestamp_ms() { date +%s%3N; }

wait_journal_idle() {
  local timeout_seconds="${1:-180}" deadline values pending lag
  deadline=$((SECONDS + timeout_seconds))
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
      return 0
    fi
    sleep 1
  done
  printf 'Journal未在%s秒内排空：pending=%s lag=%s\n' \
    "$timeout_seconds" "${pending:-unknown}" "${lag:-unknown}" >&2
  return 1
}

pod_for() {
  kubectl -n "$namespace" get pod -l "app.kubernetes.io/name=$1" \
    -o jsonpath='{.items[0].metadata.name}'
}

k6_pod="$(pod_for k6)"
kubectl -n "$namespace" cp "$model_local" "$k6_pod:$model_remote"
kubectl -n "$namespace" exec "$k6_pod" -- mkdir -p "$remote_dir"
printf 'round\tfailed\tp90_ms\tp99_ms\tjournal_drain_seconds\tverdict\n' \
  >"${series_dir}/round-summary.tsv"

for round in $(seq 1 "$rounds"); do
  printf '\n候选认证 %s/%s：%s QPS\n' "$round" "$rounds" "$target_qps"
  wait_journal_idle 300
  mysql_dsn="$(kubectl -n "$namespace" get secret farm-secrets \
    -o jsonpath='{.data.mysql-dsn}' | base64 -d)"
  kubectl -n "$namespace" exec "$k6_pod" -- benchfixture \
    -mysql-dsn "$mysql_dsn" -redis-addr redis:6379 \
    -concurrency "$reset_concurrency" -profile mixed -time-profile authentic \
    -reset-input "$fixture_remote"
  unset mysql_dsn

  task_count="$(kubectl -n "$namespace" exec "$k6_pod" -- grep -c '"task_ids"' "$fixture_remote")"
  mail_count="$(kubectl -n "$namespace" exec "$k6_pod" -- grep -c '"mail_claim_id"' "$fixture_remote")"
  if [[ "$task_count" != "$connections" || "$mail_count" != "$connections" ]]; then
    printf '夹具元数据不完整：task=%s mail=%s want=%s\n' "$task_count" "$mail_count" "$connections" >&2
    exit 1
  fi

  kubectl -n "$namespace" rollout restart deployment/farm deployment/gateway deployment/social
  for workload in deployment/farm deployment/gateway deployment/social; do
    kubectl -n "$namespace" rollout status "$workload" --timeout=8m
  done
  wait_journal_idle 300
  sleep "$settle_seconds"

  mapfile -t gateway_ips < <(
    kubectl -n "$namespace" get pod -l app.kubernetes.io/name=gateway \
      -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' | sort
  )
  if [[ "${#gateway_ips[@]}" != "3" ]]; then
    printf '需要3个Gateway Pod，实际%s个\n' "${#gateway_ips[@]}" >&2
    exit 1
  fi
  gateway_urls=""
  for ip in "${gateway_ips[@]}"; do
    [[ -n "$gateway_urls" ]] && gateway_urls+=","
    gateway_urls+="ws://${ip}:9002/ws"
  done

  result_remote="${remote_dir}/round-${round}.json"
  result_local="${series_dir}/round-${round}.json"
  kubectl -n "$namespace" exec "$k6_pod" -- servicebench \
    -mode gateway-mixed -accounts "$fixture_remote" -behavior-model "$model_remote" \
    -gateway-urls "$gateway_urls" -qps "$target_qps" -duration "${duration_seconds}s" \
    -concurrency "$connections" -fixed-connections "$connections" \
    -warmup-concurrency 256 -warmup-settle "${state_seconds}s" \
    -output "$result_remote" >/dev/null
  kubectl -n "$namespace" cp "$k6_pod:$result_remote" "$result_local"

  drain_started="$(timestamp_ms)"
  wait_journal_idle 60
  drain_ended="$(timestamp_ms)"
  python3 "${repo_root}/bench/api/capacity_slo.py" \
    --model "$model_local" --result "$result_local" --screening \
    --output "${series_dir}/round-${round}-slo.json"

  python3 - "$round" "$result_local" "${series_dir}/round-${round}-slo.json" \
    "$drain_started" "$drain_ended" >>"${series_dir}/round-summary.tsv" <<'PY'
import json
import sys

round_no, result_path, slo_path, drain_start, drain_end = sys.argv[1:]
result = json.load(open(result_path, encoding="utf-8"))
slo = json.load(open(slo_path, encoding="utf-8"))
print(
    round_no,
    result["failed"],
    result["p90_ms"],
    result["p99_ms"],
    (int(drain_end) - int(drain_start)) / 1000,
    slo["verdict"],
    sep="\t",
)
PY
done

result_args=(--result "$base_result")
for result in "${series_dir}"/round-*.json; do
  result_args+=(--result "$result")
done
python3 "${repo_root}/bench/api/capacity_slo.py" \
  --model "$model_local" "${result_args[@]}" \
  --output "${series_dir}/aggregate-slo.json"
kubectl -n "$namespace" get pods -o json >"${series_dir}/pods-after.json"
printf '\n候选认证完成：%s\n' "$series_dir"
cat "${series_dir}/round-summary.tsv"
rg '"verdict"|"minimum_samples_required"|"rounds_passed"' \
  "${series_dir}/aggregate-slo.json" | tail -12 || true
