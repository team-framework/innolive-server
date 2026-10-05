CREATE TABLE IF NOT EXISTS analytics_events (
 event_id uuid PRIMARY KEY,
 visit_id uuid NOT NULL,
 sequence bigint NOT NULL CHECK (sequence BETWEEN 1 AND 1000000),
 version integer NOT NULL CHECK (version = 1),
 event text NOT NULL CHECK (event ~ '^[a-z][a-z0-9_]{0,63}$'),
 properties jsonb NOT NULL CHECK (jsonb_typeof(properties) = 'object'),
 release text NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS analytics_events_received_at_idx ON analytics_events(received_at);
CREATE INDEX IF NOT EXISTS analytics_events_visit_sequence_idx ON analytics_events(visit_id,sequence);
