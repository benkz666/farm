#!/usr/bin/env bash
# 为10k容量复核档直接生成三份互不重叠的mixed账号和状态。
set -euo pipefail

namespace="${NAMESPACE:-benkz}"
accounts_per_shard=54201
concurrency="${RESET_CONCURRENCY:-16}"

mapfile -t k6_pods < <(
  kubectl -n "$namespace" get pod -l app.kubernetes.io/name=k6 \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort
)
if [[ "${#k6_pods[@]}" != 3 ]]; then
  printf '需要3个发压Pod，实际得到%s个\n' "${#k6_pods[@]}" >&2
  exit 1
fi

mysql_dsn="$(kubectl -n "$namespace" get secret farm-secrets -o jsonpath='{.data.mysql-dsn}' | base64 -d)"
pids=()
for shard in 0 1 2; do
  uid_base=$((3000000 + shard * 100000))
  kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- benchfixture \
    -count "$accounts_per_shard" \
    -prefix "vertical10k-s${shard}" \
    -uid-base "$uid_base" \
    -mysql-dsn "$mysql_dsn" \
    -redis-addr redis:6379 \
    -concurrency "$concurrency" \
    -profile mixed \
    -time-profile authentic \
    -output "/fixtures/vertical-unit-10k-shard-${shard}.json" \
    >"/tmp/vertical-unit-10k-shard-${shard}.log" 2>&1 &
  pids+=("$!")
done
unset mysql_dsn

status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done
if [[ "$status" != 0 ]]; then
  for shard in 0 1 2; do
    printf '%s\n' "--- shard ${shard} ---" >&2
    sed -n '1,160p' "/tmp/vertical-unit-10k-shard-${shard}.log" >&2 || true
  done
  exit 1
fi

for shard in 0 1 2; do
  fixture="/fixtures/vertical-unit-10k-shard-${shard}.json"
  count="$(kubectl -n "$namespace" exec "${k6_pods[$shard]}" -- grep -c '"task_ids"' "$fixture")"
  if [[ "$count" != "$accounts_per_shard" ]]; then
    printf '分片%s账号数=%s，期望=%s\n' "$shard" "$count" "$accounts_per_shard" >&2
    exit 1
  fi
done

printf '10k容量夹具准备完成：3 × %s 个账号\n' "$accounts_per_shard"
