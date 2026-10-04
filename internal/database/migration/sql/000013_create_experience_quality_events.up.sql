CREATE TABLE IF NOT EXISTS experience_quality_events (
 attempt_id uuid NOT NULL,
 event varchar(24) NOT NULL CHECK (event IN ('started','transport_connected','first_frame','failed','ended','cancelled')),
 version smallint NOT NULL CHECK (version = 1),
 role varchar(6) NOT NULL CHECK (role IN ('member','guest')),
 locale varchar(2) NOT NULL CHECK (locale IN ('ko','en','ja')),
 retry boolean NOT NULL,
 stage varchar(16) NOT NULL CHECK (stage IN ('session','queue','camera','signaling','transport','first_frame','streaming')),
 elapsed_ms bigint NOT NULL CHECK (elapsed_ms BETWEEN 0 AND 86400000),
 code varchar(24),
 browser varchar(7) NOT NULL CHECK (browser IN ('safari','chrome','firefox','edge','other','unknown')),
 release varchar(64) NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (attempt_id, event),
 CHECK ((event = 'failed' AND code IS NOT NULL AND code IN ('permission_denied','camera_missing','timeout','request_failed','connection_failed')) OR (event <> 'failed' AND code IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_experience_quality_received_at ON experience_quality_events(received_at);
