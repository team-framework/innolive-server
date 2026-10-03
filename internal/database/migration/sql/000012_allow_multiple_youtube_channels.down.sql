BEGIN;

DROP INDEX IF EXISTS uidx_streaming_user_youtube_channel;
DROP INDEX IF EXISTS uidx_streaming_user_chzzk;

-- 유튜브 채널을 여러 개 연결한 사용자가 있으면 실패한다. 먼저 연결을 정리해야 한다.
ALTER TABLE streaming_accounts
    ADD CONSTRAINT uidx_streaming_user_provider UNIQUE (user_id, provider);

COMMIT;
