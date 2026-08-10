BEGIN;

ALTER TABLE streaming_accounts
    DROP COLUMN IF EXISTS reconnect_required_at;

COMMIT;
