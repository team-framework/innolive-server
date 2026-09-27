BEGIN;

ALTER TABLE usage_sessions
    DROP COLUMN IF EXISTS resolution;

COMMIT;
