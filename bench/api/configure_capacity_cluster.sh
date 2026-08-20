#!/usr/bin/env bash
set -euo pipefail

namespace="${NAMESPACE:-benkz}"

wait_journal_idle() {
  local attempt values pending lag journal_pod="redis-journal-0"
  if ! kubectl -n "$namespace" get pod "$journal_pod" >/dev/null 2>&1; then
    journal_pod="redis-0"
  fi
  for attempt in $(seq 1 180); do
    values="$(
      kubectl -n "$namespace" exec "$journal_pod" -- sh -lc '
        for key in $(redis-cli --scan --pattern "*:events"); do
          redis-cli --raw XINFO GROUPS "$key"
        done | awk '\''
          $0 == "pending" { getline; pending += $0 }
          $0 == "lag" { getline; lag += $0 }
          END { print pending + 0, lag + 0 }
        '\''
      '
    )"
    read -r pending lag <<<"$values"
    if [[ "${pending:-1}" == "0" && "${lag:-1}" == "0" ]]; then
      return
    fi
    sleep 1
  done
  printf 'journal未在180秒内排空：pending=%s lag=%s\n' "$pending" "$lag" >&2
  return 1
}

wait_journal_idle

# 先降低发压Pod申请，释放调度空间，再提高被测服务资源。
kubectl -n "$namespace" set resources deployment/k6 \
  --requests=cpu=12,memory=12Gi --limits=cpu=12,memory=12Gi

kubectl -n "$namespace" scale deployment/gateway --replicas=3
kubectl -n "$namespace" scale deployment/farm --replicas=1
kubectl -n "$namespace" scale deployment/social --replicas=1
kubectl -n "$namespace" patch deployment/gateway --type=merge -p \
  '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":1,"maxSurge":0}}}}'
kubectl -n "$namespace" patch deployment/social --type=merge -p \
  '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":1,"maxSurge":0}}}}'

kubectl -n "$namespace" set resources deployment/gateway \
  --requests=cpu=2,memory=2Gi --limits=cpu=2,memory=2Gi
kubectl -n "$namespace" set env deployment/gateway GOMAXPROCS=2 GOMEMLIMIT=1536MiB

kubectl -n "$namespace" set resources deployment/farm \
  --requests=cpu=4,memory=4Gi --limits=cpu=4,memory=4Gi
kubectl -n "$namespace" set env deployment/farm \
  GOMAXPROCS=4 GOMEMLIMIT=3GiB FARM_ACTOR_MAX_RESIDENT=20000 \
  FARM_WRITE_JOURNAL_SHARDS=32 FARM_WRITE_JOURNAL_PROJECTORS=12

kubectl -n "$namespace" set resources deployment/social \
  --requests=cpu=2,memory=2Gi --limits=cpu=2,memory=2Gi
kubectl -n "$namespace" set env deployment/social GOMAXPROCS=2 GOMEMLIMIT=1536MiB

kubectl -n "$namespace" set resources statefulset/mysql \
  --requests=cpu=4,memory=6Gi --limits=cpu=4,memory=6Gi
kubectl -n "$namespace" set resources statefulset/redis \
  --requests=cpu=2,memory=3Gi --limits=cpu=2,memory=3Gi

for workload in \
  deployment/k6 deployment/gateway deployment/farm deployment/social \
  statefulset/mysql statefulset/redis; do
  kubectl -n "$namespace" rollout status "$workload" --timeout=8m
done

kubectl -n "$namespace" get deployment gateway farm social k6
kubectl -n "$namespace" get statefulset mysql redis
