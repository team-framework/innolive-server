BEGIN;

CREATE TABLE usage_pauses (
    id UUID PRIMARY KEY,
    broadcast_id UUID NOT NULL
        REFERENCES usage_broadcasts (id)
        ON UPDATE CASCADE
        ON DELETE CASCADE,
    paused_at TIMESTAMPTZ NOT NULL,
    resumed_at TIMESTAMPTZ
);

CREATE INDEX idx_usage_pauses_broadcast_id
    ON usage_pauses (broadcast_id);

COMMIT;
