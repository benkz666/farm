#!/usr/bin/env bash
set -euo pipefail

manifest_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if command -v k3s >/dev/null 2>&1; then
  kube=(k3s kubectl)
else
  kube=(kubectl)
fi

"${kube[@]}" apply -f "$manifest_dir/00-namespace.yaml"
"${kube[@]}" apply -f "$manifest_dir/10-data.yaml"
# 旧版曾部署独立事件 Redis；新架构只保留共享 redis，显式清理不再被
# manifest 管理的工作负载，但不删除任何 PVC 或数据库数据。
"${kube[@]}" -n benkz delete statefulset event-redis --ignore-not-found
"${kube[@]}" -n benkz delete service event-redis --ignore-not-found
"${kube[@]}" -n benkz delete deployment event-redis-exporter --ignore-not-found
"${kube[@]}" -n benkz rollout status statefulset/mysql --timeout=5m
"${kube[@]}" -n benkz rollout status statefulset/redis --timeout=3m

"${kube[@]}" -n benkz delete job migrate --ignore-not-found
"${kube[@]}" apply -f "$manifest_dir/20-migrate.yaml"
"${kube[@]}" -n benkz wait --for=condition=complete job/migrate --timeout=5m

"${kube[@]}" apply -f "$manifest_dir/30-apps.yaml"
"${kube[@]}" apply -f "$manifest_dir/40-observability.yaml"
"${kube[@]}" apply -f "$manifest_dir/50-bench.yaml"

for deployment in social farm gateway web prometheus grafana \
  mysqld-exporter redis-exporter k6; do
  "${kube[@]}" -n benkz rollout status "deployment/$deployment" --timeout=8m
done

"${kube[@]}" -n benkz get pods -o wide
"${kube[@]}" -n benkz get services
