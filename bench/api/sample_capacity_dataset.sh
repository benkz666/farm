#!/usr/bin/env bash
# 只读采样MySQL表容量与Redis数据集，用于估算每账号静态数据占用。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
namespace="${NAMESPACE:-benkz}"
output_dir="${1:-${repo_root}/bench/results/capacity-100-v1-20260815/static-dataset}"
mkdir -p "$output_dir"

kubectl -n "$namespace" exec mysql-0 -- sh -lc '
  MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" -D"$MYSQL_DATABASE" --batch --raw --skip-column-names -e "
    SELECT TABLE_NAME, TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH, DATA_FREE
    FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = DATABASE()
    ORDER BY TABLE_NAME;
  "
' >"${output_dir}/mysql-tables.tsv"

kubectl -n "$namespace" exec mysql-0 -- sh -lc '
  MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" -D"$MYSQL_DATABASE" --batch --raw --skip-column-names -e "
    SELECT COUNT(*) FROM account;
  "
' >"${output_dir}/mysql-account-count.txt"

kubectl -n "$namespace" exec redis-0 -- redis-cli --raw INFO memory \
  >"${output_dir}/redis-info-memory.txt"
kubectl -n "$namespace" exec redis-0 -- redis-cli --raw MEMORY STATS \
  >"${output_dir}/redis-memory-stats.txt"
kubectl -n "$namespace" exec redis-0 -- redis-cli --raw DBSIZE \
  >"${output_dir}/redis-dbsize.txt"

printf 'pattern\tkeys\n' >"${output_dir}/redis-key-patterns.tsv"
for pattern in \
  'session:*' 'farm:[0-9]*' 'farm:connreg:*' 'farm:gateway:*' \
  'farm:write:*' 'friend:*' 'mail:*' 'steal_hint:*'; do
  count="$(kubectl -n "$namespace" exec redis-0 -- sh -lc \
    "redis-cli --scan --pattern '$pattern' | wc -l")"
  printf '%s\t%s\n' "$pattern" "$count" >>"${output_dir}/redis-key-patterns.tsv"
done

date +%s >"${output_dir}/sample-unix-seconds.txt"
printf '静态数据采样完成：%s\n' "$output_dir"
