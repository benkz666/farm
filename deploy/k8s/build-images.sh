#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

# This host's Docker 24 BuildKit daemon can stall before loading even a tiny
# context. Keep the working legacy builder as the default, while allowing an
# upgraded host to opt back in with DOCKER_BUILDKIT=1.
export DOCKER_BUILDKIT="${DOCKER_BUILDKIT:-0}"

docker build -t benkz/gateway:k3s \
  --build-arg PACKAGE=./cmd/gateway \
  -f deploy/Dockerfile.server .
docker build -t benkz/farm:k3s \
  --build-arg PACKAGE=./cmd/farmsvr \
  -f deploy/Dockerfile.server .
docker build -t benkz/social:k3s \
  --build-arg PACKAGE=./cmd/socialsvr \
  -f deploy/Dockerfile.server .
docker build -t benkz/migrate:k3s -f deploy/Dockerfile.migrate .
docker build -t benkz/web:k3s -f deploy/k8s/Dockerfile.client .
docker build -t benkz/prometheus:k3s -f deploy/k8s/Dockerfile.prometheus .
docker build -t benkz/grafana:k3s -f deploy/observability/Dockerfile.grafana .
docker build -t benkz/k6:k3s -f bench/k6/Dockerfile .

for image in \
  mysql:8.4 \
  redis:7-alpine \
  prom/mysqld-exporter:v0.14.0 \
  oliver006/redis_exporter:v1.62.0; do
  docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
done

archive="$(mktemp /tmp/benkz-k3s-images.XXXXXX.tar)"
trap 'rm -f "$archive"' EXIT
docker save -o "$archive" \
  benkz/gateway:k3s \
  benkz/farm:k3s \
  benkz/social:k3s \
  benkz/migrate:k3s \
  benkz/web:k3s \
  benkz/prometheus:k3s \
  benkz/grafana:k3s \
  benkz/k6:k3s \
  mysql:8.4 \
  redis:7-alpine \
  prom/mysqld-exporter:v0.14.0 \
  oliver006/redis_exporter:v1.62.0
k3s ctr images import "$archive"
