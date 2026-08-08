#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
TARGET=${TARGET:-127.0.0.1:9204}
UID_VALUE=${UID_VALUE:-1}
PEER_UID=${PEER_UID:-2}
INTERNAL_TOKEN=${FARM_INTERNAL_TOKEN:-dev-internal-token}
QPS=${TARGET_QPS:-100}
DURATION=${DURATION:-1m}
OUTPUT=${OUTPUT:-"$ROOT/.run/bench-results/ghz-are-friends.json"}

command -v ghz >/dev/null 2>&1 || { echo "缺少 ghz，请先安装后重试" >&2; exit 1; }
mkdir -p "$(dirname "$OUTPUT")"

ghz \
  --insecure \
  --proto "$ROOT/server/proto/farm/internal/v1/social.proto" \
  --import-paths "$ROOT/server/proto" \
  --call farm.internal.v1.SocialService.AreFriends \
  --metadata "{\"authorization\":\"Bearer $INTERNAL_TOKEN\"}" \
  --data "{\"uid\":\"$UID_VALUE\",\"peerUid\":\"$PEER_UID\"}" \
  --rps "$QPS" \
  --duration "$DURATION" \
  --format json \
  --output "$OUTPUT" \
  "$TARGET"

echo "结果文件: $OUTPUT"
