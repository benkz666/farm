-- 主人侧跨农场裁决结果。必须随地块变更持久化，保证 Actor 卸载、进程重启或
-- 消息总线 at-least-once 重投后，同一个 req_id 仍会得到原始结果。
SET @missing := (
  SELECT COUNT(*) = 0 FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'player'
    AND COLUMN_NAME = 'cross_receipt_blob'
);
SET @stmt := IF(
  @missing,
  'ALTER TABLE player ADD COLUMN cross_receipt_blob VARBINARY(16384) NOT NULL DEFAULT ''''',
  'DO 0'
);
PREPARE add_cross_receipt_blob FROM @stmt;
EXECUTE add_cross_receipt_blob;
DEALLOCATE PREPARE add_cross_receipt_blob;
