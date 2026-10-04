-- Read-only report for trials started in the last seven days.
-- started is the denominator; attempts without it are reported separately.
BEGIN READ ONLY;
WITH cohort AS (
 SELECT attempt_id, browser, release, retry FROM experience_quality_events
 WHERE event = 'started' AND received_at >= now() - interval '7 days'
), attempts AS (
 SELECT c.*, bool_or(e.event = 'first_frame') AS first_frame,
 bool_or(e.event = 'failed') AS failed, bool_or(e.event = 'cancelled') AS cancelled,
 bool_or(e.event IN ('failed','cancelled','ended')) AS terminal,
 max(e.elapsed_ms) FILTER (WHERE e.event = 'transport_connected') AS connected_ms,
 max(e.elapsed_ms) FILTER (WHERE e.event = 'first_frame') AS frame_ms
 FROM cohort c JOIN experience_quality_events e USING (attempt_id)
 GROUP BY c.attempt_id, c.browser, c.release, c.retry
)
SELECT browser, release, count(*) AS attempts,
 count(*) FILTER (WHERE first_frame) AS first_frame_attempts,
 round(100.0 * count(*) FILTER (WHERE first_frame) / nullif(count(*),0),2) AS success_percent,
 count(*) FILTER (WHERE failed) AS failed_attempts,
 count(*) FILTER (WHERE cancelled) AS cancelled_attempts,
 count(*) FILTER (WHERE NOT terminal) AS open_attempts,
 count(*) FILTER (WHERE retry) AS retry_attempts,
 round(100.0 * count(*) FILTER (WHERE retry AND first_frame) / nullif(count(*) FILTER (WHERE retry),0),2) AS retry_success_percent,
 percentile_cont(0.5) WITHIN GROUP (ORDER BY connected_ms) AS connection_p50_ms,
 percentile_cont(0.95) WITHIN GROUP (ORDER BY connected_ms) AS connection_p95_ms,
 percentile_cont(0.5) WITHIN GROUP (ORDER BY frame_ms) AS first_frame_p50_ms,
 percentile_cont(0.95) WITHIN GROUP (ORDER BY frame_ms) AS first_frame_p95_ms,
 percentile_cont(0.95) WITHIN GROUP (ORDER BY frame_ms-connected_ms)
 FILTER (WHERE frame_ms >= connected_ms) AS transport_to_frame_p95_ms
FROM attempts GROUP BY browser, release ORDER BY attempts DESC;

SELECT stage, code, count(*) AS failures FROM experience_quality_events
WHERE event = 'failed' AND attempt_id IN (SELECT attempt_id FROM experience_quality_events
 WHERE event = 'started' AND received_at >= now() - interval '7 days')
GROUP BY stage, code ORDER BY failures DESC;

SELECT count(DISTINCT e.attempt_id) AS orphan_attempts FROM experience_quality_events e
WHERE e.received_at >= now() - interval '7 days'
AND NOT EXISTS (SELECT 1 FROM experience_quality_events s WHERE s.attempt_id = e.attempt_id AND s.event = 'started');
COMMIT;
