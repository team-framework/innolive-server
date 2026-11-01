BEGIN;

CREATE TABLE IF NOT EXISTS usage_local_takes (
    take_id          UUID PRIMARY KEY,
    user_id          UUID,
    device_id        VARCHAR(64) NOT NULL,
    started_at       TIMESTAMPTZ NOT NULL,
    ended_at         TIMESTAMPTZ NOT NULL,
    duration_seconds BIGINT NOT NULL,
    received_at      TIMESTAMPTZ NOT NULL,

    CONSTRAINT fk_usage_local_takes_user
        FOREIGN KEY (user_id)
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_usage_local_takes_user_started
    ON usage_local_takes (user_id, started_at);

COMMIT;
