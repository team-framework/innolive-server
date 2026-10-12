BEGIN;

ALTER TABLE usage_broadcasts
    ADD COLUMN platform_broadcast_id VARCHAR(64),
    ADD COLUMN platform_started_at TIMESTAMPTZ,
    ADD COLUMN platform_ended_at TIMESTAMPTZ;

COMMIT;
