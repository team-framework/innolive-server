BEGIN;

ALTER TABLE usage_broadcasts
    DROP COLUMN IF EXISTS platform_broadcast_id,
    DROP COLUMN IF EXISTS platform_started_at,
    DROP COLUMN IF EXISTS platform_ended_at;

COMMIT;
