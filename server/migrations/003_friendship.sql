-- 003_friendship.sql
-- 期 3：好友关系表（specs 2026-07-26-phase3-room-sync-friends.md 4.1 节）。
-- 约定 uid_lo = min(a,b)、uid_hi = max(a,b)，单条记录表达双向好友关系。
-- 主键冲突视为已存在（幂等加好友），由调用方映射为 ERR_ALREADY_FRIEND(1402)。

CREATE TABLE IF NOT EXISTS friendship (
  uid_lo     BIGINT UNSIGNED NOT NULL,
  uid_hi     BIGINT UNSIGNED NOT NULL,
  created_at BIGINT          NOT NULL,
  PRIMARY KEY (uid_lo, uid_hi),
  KEY idx_friendship_hi (uid_hi),
  KEY idx_friendship_lo (uid_lo)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
