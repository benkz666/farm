-- 012_codex_progress.sql
-- 每种作物独立累计“成功收获动作次数”，用于木牌/铜牌/银牌/金牌进度。
-- mail.source_key 为系统奖励提供幂等键；NULL 允许普通邮件继续重复创建。

CREATE TABLE IF NOT EXISTS player_codex (
  uid            BIGINT UNSIGNED NOT NULL,
  crop_id        SMALLINT UNSIGNED NOT NULL,
  harvest_count  INT UNSIGNED NOT NULL,
  updated_at     BIGINT NOT NULL,
  PRIMARY KEY (uid, crop_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

SET @mail_has_source_key := (
  SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mail' AND COLUMN_NAME = 'source_key'
);
SET @mail_add_source_key := IF(
  @mail_has_source_key = 0,
  'ALTER TABLE mail ADD COLUMN source_key VARCHAR(96) NULL AFTER uid',
  'DO 0'
);
PREPARE add_mail_source_key FROM @mail_add_source_key;
EXECUTE add_mail_source_key;
DEALLOCATE PREPARE add_mail_source_key;

SET @mail_has_source_index := (
  SELECT COUNT(*) FROM information_schema.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mail' AND INDEX_NAME = 'uq_mail_uid_source'
);
SET @mail_add_source_index := IF(
  @mail_has_source_index = 0,
  'ALTER TABLE mail ADD UNIQUE KEY uq_mail_uid_source (uid, source_key)',
  'DO 0'
);
PREPARE add_mail_source_index FROM @mail_add_source_index;
EXECUTE add_mail_source_index;
DEALLOCATE PREPARE add_mail_source_index;
