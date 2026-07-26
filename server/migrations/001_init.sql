-- 001_init.sql
-- 期 1 联通切片所需的三张表：account / player / farm_plot。
-- 依据：docs/superpowers/specs/2026-07-26-phase1-login-farm-snapshot.md 5.1 节
-- （与 docs/design/architecture.md 存储模型对齐，字段布局避免期 2 推翻）。

CREATE TABLE IF NOT EXISTS account (
  uid            BIGINT UNSIGNED PRIMARY KEY,
  username       VARCHAR(32) NOT NULL UNIQUE,
  password_hash  VARCHAR(255) NOT NULL,
  created_at     BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS player (
  uid              BIGINT UNSIGNED PRIMARY KEY,
  nickname         VARCHAR(32) NOT NULL,
  level            SMALLINT UNSIGNED NOT NULL,
  exp              INT UNSIGNED NOT NULL,
  coin             BIGINT NOT NULL,
  unlocked_plots   TINYINT UNSIGNED NOT NULL,
  -- 以下为架构对齐的占位，期 1 写默认空值即可
  codex_bitmap     BINARY(8) NOT NULL,
  friend_ids       VARBINARY(1600) NOT NULL,
  daily_blob       VARBINARY(64) NOT NULL,
  pet_blob         VARBINARY(64) NOT NULL,
  farm_seq         BIGINT UNSIGNED NOT NULL,
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS farm_plot (
  uid         BIGINT UNSIGNED NOT NULL,
  plot_index  TINYINT UNSIGNED NOT NULL,
  `blob`      VARBINARY(256) NOT NULL, -- `blob` 是 MySQL 保留字，需反引号转义
  PRIMARY KEY (uid, plot_index)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
