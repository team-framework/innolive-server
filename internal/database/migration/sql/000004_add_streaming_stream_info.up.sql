BEGIN;

ALTER TABLE streaming_accounts
    ADD COLUMN stream_id                      VARCHAR(255),
    ADD COLUMN ingestion_address              TEXT,
    ADD COLUMN backup_ingestion_address       TEXT,
    ADD COLUMN rtmps_ingestion_address        TEXT,
    ADD COLUMN rtmps_backup_ingestion_address TEXT,
    ADD COLUMN stream_name_ciphertext         BYTEA,
    ADD COLUMN stream_name_key_version        SMALLINT;

COMMIT;
