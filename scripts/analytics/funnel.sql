-- Read-only, visit-based ordered funnels. A sequence reflects client order,
-- not beacon arrival order. Keep zeros; never call these counts unique people.
WITH cohort AS (
 SELECT visit_id, min(sequence) AS landing
 FROM analytics_events
 WHERE event = 'landing_viewed' AND received_at >= now() - interval '7 days'
 GROUP BY visit_id
),
trial_start AS (
 SELECT c.*, (SELECT min(e.sequence) FROM analytics_events e WHERE e.visit_id=c.visit_id AND e.event='experience_started' AND e.sequence>c.landing) AS started FROM cohort c
),
trial_frame AS (
 SELECT c.*, (SELECT min(e.sequence) FROM analytics_events e WHERE e.visit_id=c.visit_id AND e.event='experience_succeeded' AND e.sequence>c.started) AS succeeded FROM trial_start c
),
trial_signup AS (
 SELECT c.*, (SELECT min(e.sequence) FROM analytics_events e WHERE e.visit_id=c.visit_id AND e.event='signup_completed' AND e.sequence>c.succeeded) AS completed FROM trial_frame c
),
signup_view AS (
 SELECT c.*, (SELECT min(e.sequence) FROM analytics_events e WHERE e.visit_id=c.visit_id AND e.event='signup_viewed' AND e.sequence>c.landing) AS viewed FROM cohort c
),
signup_sent AS (
 SELECT c.*, (SELECT min(e.sequence) FROM analytics_events e WHERE e.visit_id=c.visit_id AND e.event='signup_verification_sent' AND e.sequence>c.viewed) AS sent FROM signup_view c
),
signup_complete AS (
 SELECT c.*, (SELECT min(e.sequence) FROM analytics_events e WHERE e.visit_id=c.visit_id AND e.event='signup_completed' AND e.sequence>c.sent) AS completed FROM signup_sent c
),
counts AS (
 SELECT 'trial_signup' AS funnel, 1 AS step, 'landing_viewed' AS stage, count(*) AS visits FROM cohort
 UNION ALL SELECT 'trial_signup',2,'experience_started',count(started) FROM trial_signup
 UNION ALL SELECT 'trial_signup',3,'experience_succeeded',count(succeeded) FROM trial_signup
 UNION ALL SELECT 'trial_signup',4,'signup_completed',count(completed) FROM trial_signup
 UNION ALL SELECT 'direct_signup',1,'landing_viewed',count(*) FROM cohort
 UNION ALL SELECT 'direct_signup',2,'signup_viewed',count(viewed) FROM signup_complete
 UNION ALL SELECT 'direct_signup',3,'signup_verification_sent',count(sent) FROM signup_complete
 UNION ALL SELECT 'direct_signup',4,'signup_completed',count(completed) FROM signup_complete
)
SELECT funnel, step, stage, visits,
 round(100.0 * visits / nullif(first_value(visits) OVER (PARTITION BY funnel ORDER BY step),0),2) AS cohort_conversion_percent,
 round(100.0 * visits / nullif(lag(visits) OVER (PARTITION BY funnel ORDER BY step),0),2) AS previous_step_conversion_percent
FROM counts ORDER BY funnel,step;
