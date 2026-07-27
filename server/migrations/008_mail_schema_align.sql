-- 本地/演示库可能残留设计稿早期的 mail 表（mail_id/type/expires_at），
-- 与期 4 store 使用的 id 主键列冲突；CREATE IF NOT EXISTS 无法纠正。
-- 邮件为可重建的奖励投递，对齐时直接重建为期 4 形状。
DROP TABLE IF EXISTS mail;
CREATE TABLE mail (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid              BIGINT UNSIGNED NOT NULL,
  title            VARCHAR(128) NOT NULL,
  attachment_coin  BIGINT NOT NULL DEFAULT 0,
  claimed_at       BIGINT NULL,
  created_at       BIGINT NOT NULL,
  PRIMARY KEY (id),
  KEY idx_mail_uid_created (uid, created_at DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
