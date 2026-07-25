BEGIN;

CREATE TABLE users (
    id                UUID PRIMARY KEY,
    email             VARCHAR(320),
    display_name      VARCHAR(100),
    profile_image_url TEXT,
    status            VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,

    CONSTRAINT chk_users_status
        CHECK (status IN ('active', 'disabled', 'deleted'))
);

CREATE INDEX idx_users_status
    ON users (status);

CREATE TABLE oauth_accounts (
    id                                UUID PRIMARY KEY,
    user_id                           UUID NOT NULL,
    provider                          VARCHAR(20) NOT NULL,
    provider_subject                  VARCHAR(255) NOT NULL,
    provider_email                    VARCHAR(320),
    email_verified                    BOOLEAN NOT NULL DEFAULT FALSE,
    is_private_email                  BOOLEAN NOT NULL DEFAULT FALSE,
    provider_refresh_token_ciphertext BYTEA,
    provider_token_key_version        SMALLINT,
    last_login_at                     TIMESTAMPTZ NOT NULL,
    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,

    CONSTRAINT fk_oauth_accounts_user
        FOREIGN KEY (user_id)
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE CASCADE,

    CONSTRAINT chk_oauth_provider
        CHECK (provider IN ('google', 'apple')),

    CONSTRAINT uidx_oauth_provider_subject
        UNIQUE (provider, provider_subject),

    CONSTRAINT uidx_oauth_user_provider
        UNIQUE (user_id, provider)
);

CREATE INDEX idx_oauth_accounts_user_id
    ON oauth_accounts (user_id);

CREATE TABLE refresh_sessions (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL,
    family_id      UUID NOT NULL,
    token_hash     BYTEA NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    last_used_at   TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ,
    revoke_reason  VARCHAR(100),
    replaced_by_id UUID,
    user_agent     TEXT,
    ip_address     INET,
    created_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT fk_refresh_sessions_user
        FOREIGN KEY (user_id)
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE CASCADE,

    CONSTRAINT fk_refresh_sessions_replaced_by
        FOREIGN KEY (replaced_by_id)
        REFERENCES refresh_sessions (id)
        ON UPDATE CASCADE
        ON DELETE SET NULL,

    CONSTRAINT chk_refresh_token_hash_length
        CHECK (OCTET_LENGTH(token_hash) = 32),

    CONSTRAINT uni_refresh_sessions_token_hash
        UNIQUE (token_hash)
);

CREATE INDEX idx_refresh_user_expiry
    ON refresh_sessions (user_id, expires_at);

CREATE INDEX idx_refresh_sessions_family_id
    ON refresh_sessions (family_id);

CREATE INDEX idx_refresh_sessions_revoked_at
    ON refresh_sessions (revoked_at);

COMMIT;
