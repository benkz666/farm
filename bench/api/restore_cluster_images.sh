#!/usr/bin/env bash
# 环境恢复：把 docker 本地镜像重新导入 k3s containerd。
# 背景：节点曾进入 DiskPressure，kubelet 的 image GC 回收了所有本地镜像，
# 而 manifest 使用 imagePullPolicy: Never，因此需要手工回灌。
set -uo pipefail

# 实验必需镜像；web 与 grafana 不参与容量实验，故不回灌以节省磁盘。
REQUIRED=(
  "benkz/farm:k3s"
  "benkz/gateway:k3s"
  "benkz/social:k3s"
  "benkz/k6:k3s"
  "benkz/prometheus:k3s"
  "benkz/migrate:k3s"
)

missing=()
imported=()

for img in "${REQUIRED[@]}"; do
  if ! docker image inspect "$img" >/dev/null 2>&1; then
    missing+=("$img")
    continue
  fi
  if docker save "$img" | k3s ctr images import - >/dev/null 2>&1; then
    imported+=("$img")
    echo "imported: $img"
  else
    echo "IMPORT_FAILED: $img"
  fi
done

echo "---"
echo "imported_count=${#imported[@]}"
if [ ${#missing[@]} -gt 0 ]; then
  echo "MISSING_IN_DOCKER: ${missing[*]}"
fi
k3s ctr images ls -q 2>/dev/null | grep -c benkz | xargs echo "benkz_images_in_containerd="
