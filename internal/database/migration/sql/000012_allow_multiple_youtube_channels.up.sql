BEGIN;

ALTER TABLE streaming_accounts DROP CONSTRAINT IF EXISTS uidx_streaming_user_provider;
DROP INDEX IF EXISTS uidx_streaming_user_provider;

CREATE UNIQUE INDEX uidx_streaming_user_chzzk
    ON streaming_accounts (user_id)
    WHERE provider = 'chzzk';

CREATE UNIQUE INDEX uidx_streaming_user_youtube_channel
    ON streaming_accounts (user_id, channel_id)
    WHERE provider = 'youtube';

COMMIT;
