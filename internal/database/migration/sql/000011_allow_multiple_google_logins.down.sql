BEGIN;

DROP INDEX IF EXISTS uidx_oauth_user_apple;

-- 구글을 여러 개 연결한 사용자가 있으면 실패한다. 먼저 연결을 정리해야 한다.
ALTER TABLE oauth_accounts
    ADD CONSTRAINT uidx_oauth_user_provider UNIQUE (user_id, provider);

COMMIT;
