BEGIN;

ALTER TABLE streaming_accounts
    ADD COLUMN reconnect_required_at TIMESTAMPTZ;

COMMIT;
