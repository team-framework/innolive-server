BEGIN;

ALTER TABLE oauth_accounts DROP CONSTRAINT IF EXISTS uidx_oauth_user_provider;
DROP INDEX IF EXISTS uidx_oauth_user_provider;

CREATE UNIQUE INDEX uidx_oauth_user_apple
    ON oauth_accounts (user_id)
    WHERE provider = 'apple';

COMMIT;
