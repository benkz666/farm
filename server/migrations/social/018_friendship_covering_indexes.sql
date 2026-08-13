-- FriendList 冷回源会分别按 uid_lo、uid_hi 扫描好友关系，同时读取
-- created_at 和另一侧 UID。覆盖索引避免每条关系回表读取完整聚簇记录。
ALTER TABLE friendship
  DROP INDEX idx_friendship_lo,
  DROP INDEX idx_friendship_hi,
  ADD KEY idx_friendship_lo_created_peer (uid_lo, created_at, uid_hi),
  ADD KEY idx_friendship_hi_created_peer (uid_hi, created_at, uid_lo);
