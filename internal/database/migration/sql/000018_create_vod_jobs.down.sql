-- Disable VOD and stop its processes before rollback. Files must be purged and
-- deletion audit records exported before dropping these VOD-only tables.
DROP TABLE IF EXISTS vod_deletion_logs;
DROP TABLE IF EXISTS vod_jobs;
-- Their indexes are dropped with the tables; no existing table is altered.
