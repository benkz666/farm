#!/usr/bin/env bash
set -euo pipefail

namespace="${NAMESPACE:-benkz}"
source_fixture="${SOURCE_FIXTURE:-/fixtures/gateway-connection-150000.json}"
k6_pod="$(kubectl -n "$namespace" get pod -l app.kubernetes.io/name=k6 -o jsonpath='{.items[0].metadata.name}')"

make_fixture() {
  local output="$1" offset="$2" limit="$3"
  kubectl -n "$namespace" exec "$k6_pod" -- benchfixture \
    -normalize-mixed-input "$source_fixture" \
    -account-offset "$offset" \
    -account-limit "$limit" \
    -output "$output"
}

# 三个分片使用互不重叠的账号区间。1倍是各2倍分片的前半部分。
make_fixture /fixtures/vertical-unit-1x-shard-0.json 0 16261
make_fixture /fixtures/vertical-unit-1x-shard-1.json 32521 16261
make_fixture /fixtures/vertical-unit-1x-shard-2.json 65042 16261
make_fixture /fixtures/vertical-unit-2x-shard-0.json 0 32521
make_fixture /fixtures/vertical-unit-2x-shard-1.json 32521 32521
make_fixture /fixtures/vertical-unit-2x-shard-2.json 65042 32521

kubectl -n "$namespace" exec "$k6_pod" -- sh -lc \
  'ls -lh /fixtures/vertical-unit-*-shard-*.json'
