-- 期 4：薄任务、邮件附件与每日登录领取。
-- 唯一键在并发重试时维持每日登录和任务奖励的幂等边界。

CREATE TABLE IF NOT EXISTS player_task (
  uid          BIGINT UNSIGNED NOT NULL,
  logic_day    BIGINT NOT NULL,
  task_id      TINYINT UNSIGNED NOT NULL,
  progress     INT UNSIGNED NOT NULL,
  target       INT UNSIGNED NOT NULL,
  reward_coin  BIGINT NOT NULL,
  claimed_at   BIGINT NULL,
  PRIMARY KEY (uid, logic_day, task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

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

CREATE TABLE IF NOT EXISTS daily_login (
  uid        BIGINT UNSIGNED NOT NULL,
  logic_day  BIGINT NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (uid, logic_day)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
