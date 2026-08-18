#!/usr/bin/env bash
# 阶段 0：容量实验环境就绪
#
# 背景：节点曾因 MySQL binlog 无限累积（56 个文件约 49 GiB）触发 kubelet
# DiskPressure 驱逐，业务镜像被 image GC 回收，集群整体不可用。
# 本脚本完成恢复并消除复发条件，不删除任何业务数据（farm 库完整保留）。
#
# 步骤：
#   1) 应用 binlog 限额配置并重启 MySQL，使其按保留策略自动回收历史 binlog
#   2) 重启业务工作负载，使其使用已重新导入 containerd 的镜像
#   3) 健康检查与磁盘复核
set -uo pipefail

NS="${NAMESPACE:-benkz}"
MANIFEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../deploy/k8s" && pwd)"

log() { printf '[stage0] %s\n' "$*"; }

log "disk before: $(df -h / | tail -1 | awk '{print $4" avail ("$5" used)"}')"

log "applying data-tier manifest (binlog retention limits)"
kubectl apply -f "$MANIFEST_DIR/10-data.yaml" >/dev/null

log "restarting MySQL so retention policy takes effect and purges expired binlogs"
kubectl -n "$NS" rollout restart statefulset/mysql >/dev/null
kubectl -n "$NS" rollout status statefulset/mysql --timeout=6m

log "restarting business workloads onto re-imported images"
for d in gateway farm social prometheus k6; do
  kubectl -n "$NS" rollout restart "deployment/$d" >/dev/null 2>&1 || log "  (skip $d)"
done

for d in gateway farm social prometheus k6; do
  if kubectl -n "$NS" rollout status "deployment/$d" --timeout=6m >/dev/null 2>&1; then
    log "  ready: $d"
  else
    log "  NOT_READY: $d"
  fi
done

log "disk after: $(df -h / | tail -1 | awk '{print $4" avail ("$5" used)"}')"
log "pod summary:"
kubectl -n "$NS" get pods --field-selector status.phase!=Failed 2>/dev/null \
  | grep -vE "Evicted|Completed" || true

READY=$(kubectl -n "$NS" get pods --field-selector status.phase=Running -o json 2>/dev/null \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print(sum(1 for p in d['items'] if all(c.get('ready') for c in (p['status'].get('containerStatuses') or [{}]))))")
log "running_ready_pods=$READY"
