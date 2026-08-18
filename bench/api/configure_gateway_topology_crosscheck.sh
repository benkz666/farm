#!/usr/bin/env bash
# 在总Gateway CPU/内存不变时切换3×2C2Gi与6×1C1Gi，量化水平拆分折扣。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
namespace="${NAMESPACE:-benkz}"
mode="${1:?用法: configure_gateway_topology_crosscheck.sh alternate|restore}"

case "$mode" in
  alternate)
    # 3x2C -> 6x1C 的滚动过程中若保留旧Pod，会短暂申请超过6核并在
    # 单节点测试机上形成调度死锁。交叉校验不要求在线升级，因此允许先
    # 释放旧Pod；最终被测总资源仍严格保持6C/6GiB。
    kubectl -n "$namespace" patch deployment/gateway --type=merge -p \
      '{"spec":{"strategy":{"rollingUpdate":{"maxSurge":0,"maxUnavailable":"100%"}}}}'
    kubectl -n "$namespace" set resources deployment/gateway \
      --requests=cpu=1,memory=1Gi --limits=cpu=1,memory=1Gi
    kubectl -n "$namespace" set env deployment/gateway \
      GOMAXPROCS=1 GOMEMLIMIT=768MiB
    kubectl -n "$namespace" scale deployment/gateway --replicas=6
    kubectl -n "$namespace" rollout status deployment/gateway --timeout=8m
    ;;
  restore)
    bash "${repo_root}/bench/api/configure_capacity_cluster.sh"
    ;;
  *)
    printf '未知模式：%s\n' "$mode" >&2
    exit 2
    ;;
esac

kubectl -n "$namespace" get deployment gateway
