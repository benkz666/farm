CREATE TABLE farm_outbox (
    event_id VARCHAR(128) NOT NULL,
    producer_uid BIGINT UNSIGNED NOT NULL,
    target_uid BIGINT UNSIGNED NOT NULL,
    kind VARCHAR(32) NOT NULL,
    payload BLOB NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    next_attempt_at BIGINT NOT NULL DEFAULT 0,
    published_at BIGINT NULL DEFAULT NULL,
    created_at BIGINT NOT NULL,
    claim_token VARCHAR(36) NULL DEFAULT NULL,
    claim_until BIGINT NULL DEFAULT NULL,
    PRIMARY KEY (event_id),
    INDEX idx_outbox_publish (published_at, next_attempt_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
