#!/usr/bin/env bash
# 按全仓唯一版本号执行迁移；schema_migrations 保证重启容器不会重复执行。
set -euo pipefail

db_host="${FARM_MYSQL_HOST:-mysql}"
db_user="${FARM_MYSQL_USER:-farm}"
db_password="${FARM_MYSQL_PASSWORD:-farm}"
db_name="${FARM_MYSQL_DATABASE:-farm}"

mysql_exec() {
  MYSQL_PWD="$db_password" mysql --protocol=TCP -h "$db_host" -u"$db_user" "$@" "$db_name"
}

mysql_exec -e '
  CREATE TABLE IF NOT EXISTS schema_migrations (
    version VARCHAR(255) NOT NULL,
    applied_at BIGINT NOT NULL,
    PRIMARY KEY (version)
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
'

applied=0
skipped=0
while IFS= read -r migration; do
  version="${migration##*/}"
  if [[ ! "$version" =~ ^[0-9]{3}_[A-Za-z0-9_]+\.sql$ ]]; then
    printf '迁移文件名不合法：%s\n' "$migration" >&2
    exit 1
  fi
  recorded="$(mysql_exec --batch --skip-column-names \
    -e "SELECT 1 FROM schema_migrations WHERE version = '${version}' LIMIT 1" </dev/null 2>/dev/null || true)"
  if [[ "$recorded" == "1" ]]; then
    ((skipped += 1))
    continue
  fi
  printf '应用迁移：%s\n' "$migration"
  mysql_exec < "$migration"
  mysql_exec -e "
    INSERT INTO schema_migrations (version, applied_at)
    VALUES ('${version}', CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3)) * 1000 AS UNSIGNED));
  "
  ((applied += 1))
# Migrations are split by domain directories, so sorting the full path would
# run auth/006 before farm/001 on a fresh database. Sort by the globally unique
# versioned basename instead.
done < <(find /migrations -type f -name '*.sql' -printf '%f\t%p\n' | sort -k1,1 | cut -f2-)

printf '数据库迁移完成：新应用 %d 个，已跳过 %d 个\n' "$applied" "$skipped"
