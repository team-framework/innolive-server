BEGIN;

CREATE TABLE usage_sessions (
    session_id    UUID PRIMARY KEY,
    user_id       UUID,
    is_guest      BOOLEAN NOT NULL,
    ai_processing VARCHAR(20),
    started_at    TIMESTAMPTZ NOT NULL,
    ended_at      TIMESTAMPTZ,
    end_reason    VARCHAR(64),
    source        VARCHAR(10) NOT NULL DEFAULT 'live',

    CONSTRAINT fk_usage_sessions_user
        FOREIGN KEY (user_id)
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE SET NULL,

    CONSTRAINT chk_usage_sessions_source
        CHECK (source IN ('live', 'backfill'))
);

CREATE INDEX idx_usage_sessions_user_started
    ON usage_sessions (user_id, started_at);

CREATE TABLE usage_broadcasts (
    id                 UUID PRIMARY KEY,
    session_id         UUID NOT NULL,
    provider           VARCHAR(20) NOT NULL,
    started_at         TIMESTAMPTZ NOT NULL,
    live_at            TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    end_reason         VARCHAR(64),
    paused_seconds     DOUBLE PRECISION NOT NULL DEFAULT 0,
    source             VARCHAR(10) NOT NULL DEFAULT 'live',
    ended_at_estimated BOOLEAN NOT NULL DEFAULT FALSE,

    CONSTRAINT fk_usage_sessions_broadcasts
        FOREIGN KEY (session_id)
        REFERENCES usage_sessions (session_id)
        ON UPDATE CASCADE
        ON DELETE CASCADE,

    CONSTRAINT chk_usage_broadcasts_source
        CHECK (source IN ('live', 'backfill'))
);

CREATE INDEX idx_usage_broadcasts_session_id
    ON usage_broadcasts (session_id);

COMMIT;
