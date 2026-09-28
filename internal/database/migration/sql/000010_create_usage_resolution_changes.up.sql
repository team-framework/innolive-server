BEGIN;

CREATE TABLE usage_resolution_changes (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL
        REFERENCES usage_sessions (session_id)
        ON UPDATE CASCADE
        ON DELETE CASCADE,
    changed_at TIMESTAMPTZ NOT NULL,
    resolution VARCHAR(10) NOT NULL
);

CREATE INDEX idx_usage_resolution_changes_session_id
    ON usage_resolution_changes (session_id);

COMMIT;
