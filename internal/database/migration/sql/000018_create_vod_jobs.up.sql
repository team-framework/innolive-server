CREATE TABLE IF NOT EXISTS vod_jobs (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL,
 status text NOT NULL CHECK (status IN ('uploaded','queued','processing','paused','done','failed','cancelled','expired')),
 progress integer NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 queued_at timestamptz NOT NULL,
 completed_at timestamptz,
 original_delete_at timestamptz,
 result_expires_at timestamptz,
 deleted_at timestamptz,
 duration_sec double precision NOT NULL,
 frames_done bigint NOT NULL DEFAULT 0,
 frames_total bigint NOT NULL DEFAULT 0,
 segments_done integer NOT NULL DEFAULT 0,
 processing_fps double precision NOT NULL DEFAULT 0,
 checkpoint_token text NOT NULL DEFAULT '',
 worker_target text NOT NULL DEFAULT '',
 failure_code text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS vod_jobs_owner_page ON vod_jobs (user_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS vod_jobs_queue ON vod_jobs (queued_at, id) WHERE status IN ('uploaded','queued','paused','processing');
CREATE INDEX IF NOT EXISTS vod_jobs_retention ON vod_jobs (original_delete_at, result_expires_at);
CREATE TABLE IF NOT EXISTS vod_deletion_logs (
 id bigserial PRIMARY KEY,
 job_id uuid NOT NULL,
 file_kind text NOT NULL,
 path_hash text NOT NULL,
 deleted_at timestamptz NOT NULL,
 reason text NOT NULL,
 UNIQUE (job_id, file_kind, path_hash)
);
