ALTER TABLE farm_outbox
    ADD COLUMN dead_lettered_at BIGINT NULL DEFAULT NULL AFTER published_at,
    ADD INDEX idx_outbox_dead_letter (dead_lettered_at, created_at);
