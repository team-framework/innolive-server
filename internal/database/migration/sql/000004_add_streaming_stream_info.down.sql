BEGIN;

ALTER TABLE streaming_accounts
    DROP COLUMN IF EXISTS stream_id,
    DROP COLUMN IF EXISTS ingestion_address,
    DROP COLUMN IF EXISTS backup_ingestion_address,
    DROP COLUMN IF EXISTS rtmps_ingestion_address,
    DROP COLUMN IF EXISTS rtmps_backup_ingestion_address,
    DROP COLUMN IF EXISTS stream_name_ciphertext,
    DROP COLUMN IF EXISTS stream_name_key_version;

COMMIT;
