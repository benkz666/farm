-- 早期设计稿遗留的 mail 表列名是 mail_id/type/expires_at，与期 4 store 期望的
-- id 主键形状不兼容；CREATE IF NOT EXISTS 无法纠正已存在的旧表。
-- 但旧表可能持有未领取的 attachment_coin，无条件 DROP 会造成不可逆的资产损失。
-- 因此本迁移对 mail 表做形态判定，绝不删除任何已有数据：
--   - 表不存在或已是新形态（有 id、无 mail_id）：仅靠 CREATE IF NOT EXISTS 补齐，不动现有数据；
--   - 表是旧形态（有 mail_id、无 id）：RENAME 为 mail_legacy_backup 留存全部旧数据，再 CREATE IF NOT EXISTS 新表；
--   - 形态异常（既非新形态也非旧形态，或备份名 mail_legacy_backup 已被占用）：
--     强制报错中止迁移，交由人工处理，绝不静默删表。
-- 用 information_schema 判定是因为 MySQL 不允许在 IF 表达式里直接嵌 DDL，
-- 必须先算出形态再经 PREPARE 选择执行路径。重复执行幂等：旧表一旦改名走开，
-- 第二次起 mail 即为新形态，进入"不动数据"分支。
SET @mail_exists := (
  SELECT COUNT(*) FROM information_schema.TABLES
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mail'
);
SET @has_id := (
  SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mail' AND COLUMN_NAME = 'id'
);
SET @has_mail_id := (
  SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mail' AND COLUMN_NAME = 'mail_id'
);
SET @backup_taken := (
  SELECT COUNT(*) FROM information_schema.TABLES
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mail_legacy_backup'
);

-- 形态判定：仅承认两种干净形态，其余一律视为异常并安全失败。
-- @is_new      : 表存在、有 id、无 mail_id  -> 已是期 4 形态，无需动
-- @is_legacy   : 表存在、有 mail_id、无 id  -> 旧设计稿形态，改名留档后建新表
-- @is_anomaly  : 表存在但不属于上述任一干净形态（含同时有 id 与 mail_id 的污染态）
SET @is_new := IF(@mail_exists > 0 AND @has_id > 0 AND @has_mail_id = 0, 1, 0);
SET @is_legacy := IF(@mail_exists > 0 AND @has_mail_id > 0 AND @has_id = 0, 1, 0);
SET @is_anomaly := IF(@mail_exists > 0 AND @is_new = 0 AND @is_legacy = 0, 1, 0);

-- 异常形态：拒绝继续，交由人工裁决。引用一个不存在的表名让 PREPARE/EXECUTE
-- 抛出 ER_NO_SUCH_TABLE 中止迁移；表名本身即错误说明，便于运维定位。
-- 绝不在此分支执行任何 DROP / RENAME / CREATE，确保数据不被触碰。
SET @anomaly_stmt := IF(@is_anomaly > 0,
  'SELECT 1 FROM `mail_shape_anomaly_refusing_to_alter_inspect_manually`',
  'DO 0');
PREPARE anomaly_guard FROM @anomaly_stmt;
EXECUTE anomaly_guard;
DEALLOCATE PREPARE anomaly_guard;

-- 旧形态且备份名已被占用：同样安全失败，避免覆盖既有备份。
SET @backup_conflict_stmt := IF(@is_legacy > 0 AND @backup_taken > 0,
  'SELECT 1 FROM `mail_legacy_backup_already_exists_refusing_to_overwrite`',
  'DO 0');
PREPARE backup_conflict_guard FROM @backup_conflict_stmt;
EXECUTE backup_conflict_guard;
DEALLOCATE PREPARE backup_conflict_guard;

-- 仅旧形态时把旧表整体改名留档（RENAME 保留全部行与结构，无数据丢失），
-- 随后由下方 CREATE IF NOT EXISTS 建空的新形态表。@rename_stmt 恒非空（旧形态时
-- 为 RENAME，否则为 DO 0），可直接 PREPARE。
SET @rename_stmt := IF(@is_legacy > 0,
  'RENAME TABLE mail TO mail_legacy_backup',
  'DO 0');
PREPARE align_rename FROM @rename_stmt;
EXECUTE align_rename;
DEALLOCATE PREPARE align_rename;

CREATE TABLE IF NOT EXISTS mail (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid              BIGINT UNSIGNED NOT NULL,
  title            VARCHAR(128) NOT NULL,
  attachment_coin  BIGINT NOT NULL DEFAULT 0,
  claimed_at       BIGINT NULL,
  created_at       BIGINT NOT NULL,
  PRIMARY KEY (id),
  KEY idx_mail_uid_created (uid, created_at DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
