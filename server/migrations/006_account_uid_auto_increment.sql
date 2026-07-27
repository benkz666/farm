-- 期 4 修复：多 Gateway 注册必须由共享数据库分配全局唯一 uid。
-- MODIFY 可重复执行，已有 uid 保持不变，后续 AUTO_INCREMENT 从当前最大值继续。

ALTER TABLE account
  MODIFY COLUMN uid BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
