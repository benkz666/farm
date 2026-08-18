#!/usr/bin/env bash
# 对固定 Gateway 总资源池执行连接阶梯。每档重启 Gateway，避免 Go 堆高水位污染
# 下一档的内存曲线，并保存资源、镜像、Pod、事件和发压端指纹。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
namespace="${NAMESPACE:-benkz}"
run_id="${1:?用法: run_gateway_connection_pool_ladder.sh <run-id> <level> [level ...]}"
shift
if (($# == 0)); then
  printf '至少提供一个总连接数档位\n' >&2
  exit 2
fi

levels=("$@")
result_root="${RESULT_ROOT:-${repo_root}/bench/results/${run_id}}"
fixture_remote="${FIXTURE_REMOTE:-/fixtures/gateway-connection-120000.json}"
duration="${DURATION:-120s}"
warmup_concurrency="${WARMUP_CONCURRENCY:-512}"
settle_seconds="${SETTLE_SECONDS:-20}"
loadgen_shards="${LOADGEN_SHARDS:-2}"
bench_local="${BENCH_LOCAL:-/tmp/farm-servicebench-connection-v2}"
bench_remote="${BENCH_REMOTE:-/tmp/servicebench-connection-v2}"
runner_local="${repo_root}/bench/service/run_gateway_connection_pool_level.sh"
runner_remote="/tmp/run_gateway_connection_pool_level.sh"

mkdir -p "$result_root"
max_level="$(printf '%s\n' "${levels[@]}" | sort -n | tail -1)"

k6_pod="$(kubectl -n "$namespace" get pod -l app.kubernetes.io/name=k6 -o jsonpath='{.items[0].metadata.name}')"
fixture_count="$(kubectl -n "$namespace" exec "$k6_pod" -- sh -c \
  "test -f '$fixture_remote' && grep -c '\"username\"' '$fixture_remote' || true")"
if [[ ! "${fixture_count:-0}" =~ ^[0-9]+$ ]] || ((fixture_count < max_level)); then
  printf '连接夹具只有%s个账号，目标至少%s；先生成独立测试账号...\n' "${fixture_count:-0}" "$max_level"
  mysql_dsn="$(kubectl -n "$namespace" get secret farm-secrets -o jsonpath='{.data.mysql-dsn}' | base64 -d)"
  kubectl -n "$namespace" exec "$k6_pod" -- benchfixture \
    -mysql-dsn "$mysql_dsn" -redis-addr redis:6379 \
    -count "$max_level" -concurrency 64 -uid-base 26000000 \
    -prefix connpool -profile default -time-profile authentic \
    -output "$fixture_remote"
  unset mysql_dsn
fi

if [[ ! -x "$bench_local" ]]; then
  printf '缺少修正后的servicebench：%s\n' "$bench_local" >&2
  exit 2
fi

sha256sum "$bench_local" "$runner_local" >"${result_root}/artifacts.sha256"
kubectl get node -o json >"${result_root}/nodes-before.json"
kubectl -n "$namespace" get deployment gateway k6 -o json >"${result_root}/deployments-before.json"
kubectl -n "$namespace" get pod -o json >"${result_root}/pods-before.json"

restore_loadgen() {
  set +e
  kubectl -n "$namespace" scale deployment/k6 --replicas=1 >/dev/null
  kubectl -n "$namespace" set resources deployment/k6 \
    --requests=cpu=12,memory=12Gi --limits=cpu=12,memory=12Gi >/dev/null
  kubectl -n "$namespace" rollout status deployment/k6 --timeout=8m >/dev/null
}
trap restore_loadgen EXIT

# 两个源Pod解除单源IP到单目标IP只有28,232个临时端口的发压上限；总发压
# CPU/内存仍保持12C/12GiB，不增加发压资源。
if ((loadgen_shards != 2)); then
  printf '当前可复现实验要求LOADGEN_SHARDS=2\n' >&2
  exit 2
fi
kubectl -n "$namespace" set resources deployment/k6 \
  --requests=cpu=6,memory=6Gi --limits=cpu=6,memory=6Gi
kubectl -n "$namespace" rollout status deployment/k6 --timeout=8m
kubectl -n "$namespace" scale deployment/k6 --replicas="$loadgen_shards"
kubectl -n "$namespace" rollout status deployment/k6 --timeout=8m
mapfile -t k6_pods < <(
  kubectl -n "$namespace" get pod -l app.kubernetes.io/name=k6 \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort
)
if ((${#k6_pods[@]} != loadgen_shards)); then
  printf '需要%s个发压Pod，实际%s个\n' "$loadgen_shards" "${#k6_pods[@]}" >&2
  exit 1
fi
for pod in "${k6_pods[@]}"; do
  kubectl -n "$namespace" cp "$bench_local" "$pod:$bench_remote"
  kubectl -n "$namespace" cp "$runner_local" "$pod:$runner_remote"
  kubectl -n "$namespace" exec "$pod" -- chmod 0755 "$bench_remote" "$runner_remote"
  printf '%s ' "$pod" >>"${result_root}/loadgen-limits.txt"
  kubectl -n "$namespace" exec "$pod" -- sh -c \
    'printf "fd="; ulimit -n; printf "ports="; cat /proc/sys/net/ipv4/ip_local_port_range; printf "memory="; cat /sys/fs/cgroup/memory.max; printf "cpu="; cat /sys/fs/cgroup/cpu.max' \
    >>"${result_root}/loadgen-limits.txt"
done

for level in "${levels[@]}"; do
  level_dir="${result_root}/level-${level}"
  remote_dir="/results/${run_id}/level-${level}"
  mkdir -p "$level_dir"
  printf '\nGateway总资源池连接档：%s\n' "$level"

  kubectl -n "$namespace" rollout restart deployment/gateway
  kubectl -n "$namespace" rollout status deployment/gateway --timeout=8m
  sleep "$settle_seconds"

  kubectl -n "$namespace" get deployment gateway -o json >"${level_dir}/deployment.json"
  kubectl -n "$namespace" get pod -l app.kubernetes.io/name=gateway -o json >"${level_dir}/pods-before.json"

  mapfile -t gateway_rows < <(
    kubectl -n "$namespace" get pod -l app.kubernetes.io/name=gateway \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.podIP}{"\n"}{end}' | sort
  )
  gateway_urls=""
  gateway_metrics=""
  for row in "${gateway_rows[@]}"; do
    read -r pod ip <<<"$row"
    [[ -n "$gateway_urls" ]] && gateway_urls+=","
    [[ -n "$gateway_metrics" ]] && gateway_metrics+=","
    gateway_urls+="ws://${ip}:9002/ws"
    gateway_metrics+="${pod}|http://${ip}:9302/metrics"
  done

  shard_pids=()
  shard_offset=0
  for ((shard=0; shard<loadgen_shards; shard++)); do
    shard_connections=$((level / loadgen_shards))
    if ((shard < level % loadgen_shards)); then
      shard_connections=$((shard_connections + 1))
    fi
    sample_metrics=0
    if ((shard == 0)); then
      sample_metrics=1
    fi
    kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- env \
      RESULT_DIR="$remote_dir" FIXTURE="$fixture_remote" DURATION="$duration" \
      WARMUP_CONCURRENCY="$warmup_concurrency" GATEWAY_URLS="$gateway_urls" \
      GATEWAY_METRICS="$gateway_metrics" BENCH_BIN="$bench_remote" \
      SHARD_CONNECTIONS="$shard_connections" ACCOUNT_OFFSET="$shard_offset" \
      SHARD_INDEX="$shard" SAMPLE_METRICS="$sample_metrics" \
      "$runner_remote" "$level" &
    shard_pids+=("$!")
    shard_offset=$((shard_offset + shard_connections))
  done
  status=0
  set +e
  for pid in "${shard_pids[@]}"; do
    wait "$pid"
    shard_status=$?
    if ((shard_status != 0)); then
      status=$shard_status
    fi
  done
  set -e

  kubectl -n "$namespace" cp "${k6_pods[0]}:$remote_dir/." "$level_dir" 2>/dev/null || true
  kubectl -n "$namespace" get pod -l app.kubernetes.io/name=gateway -o json >"${level_dir}/pods-after.json"
  kubectl -n "$namespace" get events --sort-by=.lastTimestamp >"${level_dir}/events-after.txt"
  printf '%s\n' "$status" >"${level_dir}/kubectl-exec-status.txt"
  if ((status != 0)); then
    printf '档位%s失败，已保存现场；停止继续加压。\n' "$level" >&2
    break
  fi
done

kubectl -n "$namespace" get deployment gateway k6 -o json >"${result_root}/deployments-after.json"
kubectl -n "$namespace" get pod -o json >"${result_root}/pods-after.json"
