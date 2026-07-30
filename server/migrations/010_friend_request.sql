-- 010_friend_request.sql
-- 好友申请：仅存待处理；同意后删行并写入 friendship；拒绝则删行。
-- 分享链接仍走 AcceptInvite 直接建好友，不经本表。

CREATE TABLE IF NOT EXISTS friend_request (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  from_uid   BIGINT UNSIGNED NOT NULL,
  to_uid     BIGINT UNSIGNED NOT NULL,
  created_at BIGINT          NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_friend_request_pair (from_uid, to_uid),
  KEY idx_friend_request_to (to_uid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
