BEGIN;

CREATE TABLE streaming_accounts (
    id                       UUID PRIMARY KEY,
    user_id                  UUID NOT NULL,
    provider                 VARCHAR(20) NOT NULL,
    channel_id               VARCHAR(255) NOT NULL,
    channel_title            VARCHAR(255),
    refresh_token_ciphertext BYTEA,
    token_key_version        SMALLINT,
    refresh_token_expires_at TIMESTAMPTZ,
    manual_ingest_url        TEXT,
    connected_at             TIMESTAMPTZ NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL,
    updated_at               TIMESTAMPTZ NOT NULL,

    CONSTRAINT fk_streaming_accounts_user
        FOREIGN KEY (user_id)
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE CASCADE,

    CONSTRAINT chk_streaming_provider
        CHECK (provider IN ('youtube', 'chzzk')),

    CONSTRAINT uidx_streaming_user_provider
        UNIQUE (user_id, provider)
);

CREATE INDEX idx_streaming_accounts_user_id
    ON streaming_accounts (user_id);

COMMIT;
