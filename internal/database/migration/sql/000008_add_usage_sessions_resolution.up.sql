BEGIN;

ALTER TABLE usage_sessions
    ADD COLUMN resolution VARCHAR(10);

COMMIT;
