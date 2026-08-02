-- 011_mail_read_state.sql
-- Existing inbox rows predate unread semantics. Mark them read exactly once when
-- this column is first introduced so deployment does not light every old notice.
SET @mail_has_read_at := (
  SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mail' AND COLUMN_NAME = 'read_at'
);

SET @mail_add_read_at := IF(
  @mail_has_read_at = 0,
  'ALTER TABLE mail ADD COLUMN read_at BIGINT NULL AFTER claimed_at',
  'DO 0'
);
PREPARE add_mail_read_at FROM @mail_add_read_at;
EXECUTE add_mail_read_at;
DEALLOCATE PREPARE add_mail_read_at;

-- Run only on the first application. Re-executing this migration must preserve
-- mails created after rollout as unread (read_at NULL).
SET @mail_backfill_read_at := IF(
  @mail_has_read_at = 0,
  'UPDATE mail SET read_at = created_at WHERE read_at IS NULL',
  'DO 0'
);
PREPARE backfill_mail_read_at FROM @mail_backfill_read_at;
EXECUTE backfill_mail_read_at;
DEALLOCATE PREPARE backfill_mail_read_at;
