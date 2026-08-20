#!/usr/bin/env bash
set -euo pipefail

namespace="${NAMESPACE:-benkz}"
tier="${1:?用法: configure_vertical_capacity_profile.sh <1|2|10k|15k|ceil>}"

case "$tier" in
  ceil)
    # 天花板校准档：与其它档位相反，配额在整轮上探中固定不动，只推负载。
    # 前面几档把配额和负载一起放大，测出来的是"这一档能不能过"，读不出配额的
    # 真天花板。锁死配额后 SLO 破的那一档就是单实例上限，可直接用于容量外推。
    #
    # 配比取自 1.5U 实测系数：Gateway 每核 4GiB、Farm 每核 3.3GiB。0.9U 负载下
    # 两者的 CPU 与内存利用率都落在 67~70%，四个数字彼此相差不到 3 个百分点。
    #
    # Gateway 只能给 GOGC=100：12GiB 配额下 GOGC=200 的峰值需求约 11.1GiB、占满
    # 93%。代价是 GC 轮次翻倍而 GC 只有 0.75 核可用（3 核的 25%），OPT-5 里 1.5U
    # 的 P99 超标正是 mark 阶段扫栈扫堆造成的。本档连接数少四成、mark 工作量随之
    # 下降，能否达标由 0.9U 基线判定；若不达标则必须回 GOGC=200 并把内存抬到
    # 16GiB，配比随之变成 1:5.3。
    # Farm 的软上限按配额上界给，而不是按某一档的需求给。0.9U 实测峰值 6.97GiB，
    # 1.0U 外推 7.74GiB——原来的 8GiB 会让 GC 贴着软上限疯转、把 P99 拖崩，那测到的
    # 是软上限而非配额的天花板。10Gi 配额扣掉非堆开销后可给到 8.5GiB。0.9U 未触及
    # 软上限，所以此改动不破坏两档可比性。
    gateway_cpu=3 gateway_mem=12Gi gateway_gomax=3 gateway_gomem=10GiB
    farm_cpu=3 farm_mem=10Gi farm_gomax=3 farm_gomem=8704MiB actor_limit=250000
    social_cpu=1 social_mem=512Mi social_gomax=1 social_gomem=384MiB
    farm_gogc=200
    # 数据层刻意不按最终方案的 2~4 核给。MySQL 的 CPU 是突发型的——1U 那轮平均
    # 只用 1.28/3 核却累计节流 41.7s。本档要测应用层天花板，先给到已验证不节流的
    # 6 核，避免数据层节流冒充应用层瓶颈。2 倍扩容的装箱另做一轮收缩验证。
    mysql_cpu=6 mysql_mem=6Gi mysql_buffer=5G
    redis_cpu=1 redis_mem=1792Mi
    journal_redis_cpu=1 journal_redis_mem=1Gi
    presence_redis_cpu=1 presence_redis_mem=768Mi
    # shard 数按 Farm 的 CPU 密度定（其余档位是每核 4~5 个），3 核取 15。
    # 在途写与积压水位按 QPS 定，取上探上限 1.3U（13000 QPS）以免中途变更配置。
    committer_shards=15 journal_shards=15 journal_projectors=15
    write_max=1664 write_min=208 backlog_low=26624 backlog_high=212992 backlog_hard=851968 recovery_step=208
    ;;
  1)
    gateway_cpu=3 gateway_mem=4608Mi gateway_gomax=3 gateway_gomem=3840MiB
    farm_cpu=3 farm_mem=8Gi farm_gomax=3 farm_gomem=7GiB actor_limit=70000
    social_cpu=1 social_mem=1Gi social_gomax=1 social_gomem=800MiB
    mysql_cpu=2 mysql_mem=3Gi mysql_buffer=2G
    redis_cpu=1 redis_mem=1Gi
    journal_redis_cpu=1 journal_redis_mem=768Mi
    presence_redis_cpu=1 presence_redis_mem=512Mi
    # Journal 分片过多会把连续写流打散成大量小事务；在本机 MySQL
    # innodb_flush_log_at_trx_commit=1 时，这会令前台任务/邮件写等待刷盘。
    # 以每个 Farm CPU 2 个 projector、4 个 journal shard 为密度，既保持
    # 垂直翻倍关系，又让每个事务获得足够批量。投影并发必须与 shard 对齐。
    committer_shards=12 journal_shards=12 journal_projectors=12
    write_max=768 write_min=96 backlog_low=12288 backlog_high=98304 backlog_hard=393216 recovery_step=96
    ;;
  2)
    gateway_cpu=6 gateway_mem=9Gi gateway_gomax=6 gateway_gomem=7680MiB
    farm_cpu=6 farm_mem=16Gi farm_gomax=6 farm_gomem=14GiB actor_limit=140000
    social_cpu=2 social_mem=2Gi social_gomax=2 social_gomem=1600MiB
    mysql_cpu=4 mysql_mem=6Gi mysql_buffer=4G
    redis_cpu=1 redis_mem=1536Mi
    journal_redis_cpu=1 journal_redis_mem=1Gi
    presence_redis_cpu=1 presence_redis_mem=512Mi
    committer_shards=24 journal_shards=24 journal_projectors=24
    write_max=1536 write_min=192 backlog_low=24576 backlog_high=196608 backlog_hard=786432 recovery_step=192
    ;;
  10k)
    # 10k复核档仍保持单实例拓扑。配额按预计实际占用留出余量，避免将
    # 3k档机械放大3.33倍后与三个发压Pod一起超过单节点32核。容量计算只
    # 使用Prometheus实际用量，并额外检查CPU throttling为零或可忽略。
    gateway_cpu=5 gateway_mem=12Gi gateway_gomax=5 gateway_gomem=10GiB
    farm_cpu=5 farm_mem=10Gi farm_gomax=5 farm_gomem=8GiB actor_limit=200000
    social_cpu=1 social_mem=1Gi social_gomax=1 social_gomem=800MiB
    # OPT-3：1U 复测里 MySQL 平均只用 1.28/3 核却累计节流 41.7s，是 CFS 周期内
    # 的瞬时配额撞墙；投影吞吐被它挡死。提配额到 6 核并加大 buffer pool 以减少
    # 读 IO 与脏页刷盘。节点 32 核，提完 CPU requests 约 89%，仍可调度。
    mysql_cpu=6 mysql_mem=8Gi mysql_buffer=5G
    redis_cpu=1 redis_mem=1280Mi
    journal_redis_cpu=1 journal_redis_mem=768Mi
    presence_redis_cpu=1 presence_redis_mem=512Mi
    # projector 必须与 journal shard 对齐；前台写不再收窄投影并发。
    committer_shards=20 journal_shards=20 journal_projectors=20
    write_max=1280 write_min=160 backlog_low=20480 backlog_high=163840 backlog_hard=655360 recovery_step=160
    ;;
  15k)
    # 上探档：1U 通过后按 1.5 倍上探，找单节点真天花板。配额按 1U 的
    # smoothed p95 实测乘 1.5 再留余量：Gateway 2.93、Farm 3.05、MySQL 1.72、
    # Redis 合计 1.39 核。2U 装不下——Gateway 与 Farm 各需 7 核、Redis 三台各需
    # 1.5 核，加上不可压缩的发压 7.8 核合计 35.2 核，超过节点的 32 核。
    # GOMEMLIMIT 把 goroutine 栈也算在内：18 万连接约 54 万 goroutine、栈约 4.3GB，
    # 再加约 7.2GB 堆。留到 20Gi/17GiB，避免 GC 因为逼近软上限而疯转。
    gateway_cpu=6 gateway_mem=20Gi gateway_gomax=6 gateway_gomem=18GiB
    farm_cpu=6 farm_mem=14Gi farm_gomax=6 farm_gomem=12GiB actor_limit=300000
    social_cpu=1 social_mem=1Gi social_gomax=1 social_gomem=800MiB
    # OPT-5：GC 的 mark 阶段是本档 P99 的主因，改用更大的堆目标换更少的 GC 轮次。
    # 两处配额都在已有余量内挪动（Gateway 实测峰值 11.9/20Gi，MySQL 2.7/8Gi 且
    # 全量数据仅约 1GB、buffer pool 吃不满），节点 requests 总量保持不变。
    gateway_gogc=200 farm_gogc=200
    mysql_cpu=6 mysql_mem=6Gi mysql_buffer=5G
    redis_cpu=1 redis_mem=1792Mi
    journal_redis_cpu=1 journal_redis_mem=1Gi
    presence_redis_cpu=1 presence_redis_mem=768Mi
    committer_shards=30 journal_shards=30 journal_projectors=30
    write_max=1920 write_min=240 backlog_low=30720 backlog_high=245760 backlog_hard=983040 recovery_step=240
    ;;
  *)
    printf 'tier必须是1、2、10k、15k或ceil\n' >&2
    exit 2
    ;;
esac

# 低档位堆小、GC 轮次的影响进不了 P99，保持 Go 默认值。
gateway_gogc="${gateway_gogc:-100}"
farm_gogc="${farm_gogc:-100}"

# 三个发压Pod提供三个源IP；资源不属于被测容量单元，各档保持不变。
# 曾把"连接立不起来"归因到这里的 1600m，实为发压端 Pong 轮询用逐连接定时器、
# 一圈耗时超过 Gateway 的 90s 读超时（已在 servicebench 侧改为按批推送）。
# 配额仍保持 2600m 以免掺入新变量。Gateway 改用 Recreate 后滚动期不再双占
# 配额，10k 档总 requests 约 28.7 核，本档在 32 核节点上放得下。
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
  GOMAXPROCS="$gateway_gomax" GOMEMLIMIT="$gateway_gomem" GOGC="$gateway_gogc" \
  FARM_PRESENCE_REDIS_ADDR=redis-presence:6379

kubectl -n "$namespace" set resources deployment/farm \
  --requests="cpu=${farm_cpu},memory=${farm_mem}" \
  --limits="cpu=${farm_cpu},memory=${farm_mem}"
kubectl -n "$namespace" set env deployment/farm \
  GOMAXPROCS="$farm_gomax" GOMEMLIMIT="$farm_gomem" GOGC="$farm_gogc" \
  FARM_ACTOR_MAX_RESIDENT="$actor_limit" FARM_ACTOR_IDLE_TTL=20m \
  FARM_COMMITTER_SHARDS="$committer_shards" \
  FARM_WRITE_JOURNAL_SHARDS="$journal_shards" \
  FARM_WRITE_JOURNAL_PROJECTORS="$journal_projectors" \
  FARM_EVENT_REDIS_ADDR=redis-journal:6379 \
  FARM_PRESENCE_REDIS_ADDR=redis-presence:6379 \
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
  GOMAXPROCS="$social_gomax" GOMEMLIMIT="$social_gomem" \
  FARM_PRESENCE_REDIS_ADDR=redis-presence:6379

kubectl -n "$namespace" set resources statefulset/mysql \
  --requests="cpu=${mysql_cpu},memory=${mysql_mem}" \
  --limits="cpu=${mysql_cpu},memory=${mysql_mem}"
# OPT-3：节点磁盘在 1U 下已饱和（写队列深度 116、写等待 58ms、利用率 97%）。
# trx_commit=2 与 sync_binlog=0 去掉每事务的两次 fsync，doublewrite=0 让数据页
# 写入减半。binlog 本身保留，提交的写放大成本仍计入容量结论。
#
# OPT-4：redo 容量的 100MB 默认值是 1U 下的硬墙。redo 写入约 6MB/s，180s 内逻辑
# 大小就涨到 77MB，InnoDB 被迫激进推进检查点，threads_running 从 2 冲到 22，投影
# 积压堆到 20427。此时脏页占比只有 5~7%（阈值 90%）、buffer_pool_wait_free 与
# log_waits 均为 0，所以瓶颈不在 buffer pool。给到 8GiB：2U 档约 12MB/s，600s 长稳
# 也只写 7.2GB，一轮内不必回绕。
kubectl -n "$namespace" patch statefulset/mysql --type=strategic -p "$(printf '%s' \
  '{"spec":{"template":{"spec":{"containers":[{"name":"mysql","args":["--character-set-server=utf8mb4","--max-connections=1000","--innodb-buffer-pool-size=BUFFER","--innodb-flush-log-at-trx-commit=2","--sync-binlog=0","--innodb-doublewrite=OFF","--innodb-redo-log-capacity=8589934592","--max-binlog-size=134217728","--binlog-expire-logs-seconds=1800","--transaction-isolation=READ-COMMITTED"]}]}}}}' \
  | sed "s/BUFFER/${mysql_buffer}/")"

kubectl -n "$namespace" set resources statefulset/redis \
  --requests="cpu=${redis_cpu},memory=${redis_mem}" \
  --limits="cpu=${redis_cpu},memory=${redis_mem}"
kubectl -n "$namespace" set resources statefulset/redis-journal \
  --requests="cpu=${journal_redis_cpu},memory=${journal_redis_mem}" \
  --limits="cpu=${journal_redis_cpu},memory=${journal_redis_mem}"
kubectl -n "$namespace" set resources statefulset/redis-presence \
  --requests="cpu=${presence_redis_cpu},memory=${presence_redis_mem}" \
  --limits="cpu=${presence_redis_cpu},memory=${presence_redis_mem}"

for workload in \
  deployment/k6 deployment/gateway deployment/farm deployment/social \
  statefulset/mysql statefulset/redis statefulset/redis-journal statefulset/redis-presence; do
  kubectl -n "$namespace" rollout status "$workload" --timeout=10m
done

kubectl -n "$namespace" get deployment gateway farm social k6
kubectl -n "$namespace" get statefulset mysql redis redis-journal redis-presence
