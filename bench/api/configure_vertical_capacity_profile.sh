#!/usr/bin/env bash
set -euo pipefail

namespace="${NAMESPACE:-benkz}"
tier="${1:?用法: configure_vertical_capacity_profile.sh <1|2|10k>}"

case "$tier" in
  1)
    gateway_cpu=3 gateway_mem=4608Mi gateway_gomax=3 gateway_gomem=3840MiB
    farm_cpu=3 farm_mem=8Gi farm_gomax=3 farm_gomem=7GiB actor_limit=70000
    social_cpu=1 social_mem=1Gi social_gomax=1 social_gomem=800MiB
    mysql_cpu=2 mysql_mem=3Gi mysql_buffer=2G
    redis_cpu=1 redis_mem=1536Mi
    # Journal 分片过多会把连续写流打散成大量小事务；在本机 MySQL
    # innodb_flush_log_at_trx_commit=1 时，这会令前台任务/邮件写等待刷盘。
    # 以每个 Farm CPU 2 个 projector、4 个 journal shard 为密度，既保持
    # 垂直翻倍关系，又让每个事务获得足够批量。
    committer_shards=12 journal_shards=12 journal_projectors=6
    write_max=768 write_min=96 backlog_low=12288 backlog_high=98304 backlog_hard=393216 recovery_step=96
    ;;
  2)
    gateway_cpu=6 gateway_mem=9Gi gateway_gomax=6 gateway_gomem=7680MiB
    farm_cpu=6 farm_mem=16Gi farm_gomax=6 farm_gomem=14GiB actor_limit=140000
    social_cpu=2 social_mem=2Gi social_gomax=2 social_gomem=1600MiB
    mysql_cpu=4 mysql_mem=6Gi mysql_buffer=4G
    redis_cpu=2 redis_mem=3Gi
    committer_shards=24 journal_shards=24 journal_projectors=12
    write_max=1536 write_min=192 backlog_low=24576 backlog_high=196608 backlog_hard=786432 recovery_step=192
    ;;
  10k)
    # 10k复核档仍保持单实例拓扑。配额按预计实际占用留出余量，避免将
    # 3k档机械放大3.33倍后与三个发压Pod一起超过单节点32核。容量计算只
    # 使用Prometheus实际用量，并额外检查CPU throttling为零或可忽略。
    gateway_cpu=5 gateway_mem=12Gi gateway_gomax=5 gateway_gomem=10GiB
    farm_cpu=5 farm_mem=10Gi farm_gomax=5 farm_gomem=8GiB actor_limit=200000
    social_cpu=1 social_mem=1Gi social_gomax=1 social_gomem=800MiB
    mysql_cpu=3 mysql_mem=5Gi mysql_buffer=3G
    redis_cpu=3 redis_mem=2560Mi
    # 正式10k容量结果使用10个projector。该轮停压后47.699秒排空，报告
    # 将其保留为限制；当前范围不再追加调参后的持续性复测。
    committer_shards=20 journal_shards=20 journal_projectors=10
    write_max=1280 write_min=160 backlog_low=20480 backlog_high=163840 backlog_hard=655360 recovery_step=160
    ;;
  *)
    printf 'tier必须是1、2或10k\n' >&2
    exit 2
    ;;
esac

# 三个发压Pod提供三个源IP；资源不属于被测容量单元，两档保持不变。
kubectl -n "$namespace" set resources deployment/k6 \
  --requests=cpu=2600m,memory=3Gi --limits=cpu=2600m,memory=3Gi
kubectl -n "$namespace" scale deployment/k6 --replicas=3

kubectl -n "$namespace" scale deployment/gateway --replicas=1
kubectl -n "$namespace" scale deployment/farm --replicas=1
kubectl -n "$namespace" scale deployment/social --replicas=1

kubectl -n "$namespace" set resources deployment/gateway \
  --requests="cpu=${gateway_cpu},memory=${gateway_mem}" \
  --limits="cpu=${gateway_cpu},memory=${gateway_mem}"
kubectl -n "$namespace" set env deployment/gateway \
  GOMAXPROCS="$gateway_gomax" GOMEMLIMIT="$gateway_gomem"

kubectl -n "$namespace" set resources deployment/farm \
  --requests="cpu=${farm_cpu},memory=${farm_mem}" \
  --limits="cpu=${farm_cpu},memory=${farm_mem}"
kubectl -n "$namespace" set env deployment/farm \
  GOMAXPROCS="$farm_gomax" GOMEMLIMIT="$farm_gomem" \
  FARM_ACTOR_MAX_RESIDENT="$actor_limit" FARM_ACTOR_IDLE_TTL=20m \
  FARM_COMMITTER_SHARDS="$committer_shards" \
  FARM_WRITE_JOURNAL_SHARDS="$journal_shards" \
  FARM_WRITE_JOURNAL_PROJECTORS="$journal_projectors" \
  FARM_WRITE_MAX_IN_FLIGHT="$write_max" \
  FARM_WRITE_MIN_IN_FLIGHT="$write_min" \
  FARM_WRITE_BACKLOG_LOW="$backlog_low" \
  FARM_WRITE_BACKLOG_HIGH="$backlog_high" \
  FARM_WRITE_BACKLOG_HARD="$backlog_hard" \
  FARM_WRITE_RECOVERY_STEP="$recovery_step"

kubectl -n "$namespace" set resources deployment/social \
  --requests="cpu=${social_cpu},memory=${social_mem}" \
  --limits="cpu=${social_cpu},memory=${social_mem}"
kubectl -n "$namespace" set env deployment/social \
  GOMAXPROCS="$social_gomax" GOMEMLIMIT="$social_gomem"

kubectl -n "$namespace" set resources statefulset/mysql \
  --requests="cpu=${mysql_cpu},memory=${mysql_mem}" \
  --limits="cpu=${mysql_cpu},memory=${mysql_mem}"
kubectl -n "$namespace" patch statefulset/mysql --type=strategic -p "$(printf '%s' \
  '{"spec":{"template":{"spec":{"containers":[{"name":"mysql","args":["--character-set-server=utf8mb4","--max-connections=1000","--innodb-buffer-pool-size=BUFFER","--innodb-flush-log-at-trx-commit=1"]}]}}}}' \
  | sed "s/BUFFER/${mysql_buffer}/")"

kubectl -n "$namespace" set resources statefulset/redis \
  --requests="cpu=${redis_cpu},memory=${redis_mem}" \
  --limits="cpu=${redis_cpu},memory=${redis_mem}"

for workload in \
  deployment/k6 deployment/gateway deployment/farm deployment/social \
  statefulset/mysql statefulset/redis; do
  kubectl -n "$namespace" rollout status "$workload" --timeout=10m
done

kubectl -n "$namespace" get deployment gateway farm social k6
kubectl -n "$namespace" get statefulset mysql redis
