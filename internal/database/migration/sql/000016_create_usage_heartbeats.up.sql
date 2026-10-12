BEGIN;

CREATE TABLE usage_heartbeats (
    id UUID PRIMARY KEY,
    session_id UUID NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL,
    connected_targets BIGINT NOT NULL,
    video_fps DOUBLE PRECISION NOT NULL,
    dropped_frames BIGINT NOT NULL,
    failed_frames BIGINT NOT NULL,
    platform VARCHAR(20) NOT NULL,
    payload JSONB NOT NULL
);

CREATE INDEX idx_usage_heartbeats_session_received
    ON usage_heartbeats (session_id, received_at);

COMMIT;
