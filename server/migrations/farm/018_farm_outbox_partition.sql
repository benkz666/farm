ALTER TABLE farm_outbox
    ADD COLUMN producer_farm_id VARCHAR(64) NOT NULL DEFAULT 'farm-0' AFTER event_id,
    DROP INDEX idx_outbox_publish,
    ADD INDEX idx_outbox_dispatch (
        producer_farm_id,
        published_at,
        dead_lettered_at,
        next_attempt_at,
        created_at
    );
