-- 스트리머 실사용 집계(#266). psql -f 로 실행한다. 읽기 전용이다.
--
-- 정의
--   접속 시간   : 세션(앱을 켠 구간) started_at ~ ended_at
--   방송 시간   : 송출 COALESCE(live_at, started_at) ~ ended_at - paused_seconds
--                 (유튜브는 준비 단계에 송출이 먼저 붙으므로 라이브 전환부터 센다)
--   방송 회차   : 송출이 하나 이상 있었던 세션. 동시 송출(유튜브+치지직)은 한 회차다.
--   방송 간격   : 같은 계정의 직전 회차 종료 ~ 이번 회차 시작(시간)
--
-- 주의
--   - ended_at이 NULL인 행(unclean_shutdown)은 시간 합계에서 빠진다.
--   - ended_at_estimated=true는 백필에서 세션 종료 시각으로 채운 송출이다.
--   - 간격이 수 분 이내면 재접속(네트워크 끊김 후 다시 켬)일 가능성이 크다.
--   - 팀·테스트 계정은 아직 제외하지 않는다.

\pset footer off

-- 공통 뷰 ---------------------------------------------------------------
CREATE TEMP VIEW usage_on_air AS
SELECT b.*,
       s.user_id,
       s.is_guest,
       GREATEST(EXTRACT(EPOCH FROM b.ended_at - COALESCE(b.live_at, b.started_at)) - b.paused_seconds, 0)
           AS on_air_seconds
FROM usage_broadcasts b
JOIN usage_sessions s USING (session_id);

CREATE TEMP VIEW usage_rounds AS
SELECT session_id,
       MIN(user_id::text)::uuid                        AS user_id,
       BOOL_OR(is_guest)                                AS is_guest,
       MIN(COALESCE(live_at, started_at))               AS round_start,
       MAX(ended_at)                                    AS round_end,
       SUM(on_air_seconds)                              AS on_air_seconds,
       COUNT(DISTINCT provider)                         AS providers,
       STRING_AGG(DISTINCT provider, '+')               AS platforms
FROM usage_on_air
GROUP BY session_id;

-- 1. 전체 요약 -----------------------------------------------------------
\echo '== 1. 전체 요약'
SELECT MIN(s.started_at)::date                                          AS 기간_시작,
       MAX(s.started_at)::date                                          AS 기간_끝,
       COUNT(*)                                                         AS 세션,
       COUNT(*) FILTER (WHERE NOT s.is_guest)                           AS 회원_세션,
       COUNT(DISTINCT s.user_id)                                        AS 접속_계정,
       (SELECT COUNT(*) FROM usage_rounds)                              AS 방송_회차,
       (SELECT COUNT(DISTINCT user_id) FROM usage_rounds)               AS 방송한_계정,
       (SELECT ROUND((SUM(on_air_seconds) / 3600)::numeric, 1) FROM usage_rounds) AS 총_방송_시간h,
       (SELECT ROUND((PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY on_air_seconds) / 60)::numeric, 1)
          FROM usage_rounds WHERE round_end IS NOT NULL)                AS 방송_중앙값_분,
       (SELECT COUNT(*) FROM usage_broadcasts WHERE ended_at_estimated) AS 추정_마감_송출
FROM usage_sessions s;

-- 2. 계정별 ---------------------------------------------------------------
\echo '== 2. 계정별 (방송 회차 많은 순)'
WITH gaps AS (
    SELECT user_id,
           EXTRACT(EPOCH FROM round_start - LAG(round_end) OVER (PARTITION BY user_id ORDER BY round_start)) / 3600
               AS gap_hours
    FROM usage_rounds
    WHERE user_id IS NOT NULL
),
sessions AS (
    SELECT user_id,
           COUNT(*)                                                  AS sessions,
           SUM(EXTRACT(EPOCH FROM ended_at - started_at)) / 3600     AS connected_hours,
           MIN(started_at)                                           AS first_seen,
           MAX(started_at)                                           AS last_seen
    FROM usage_sessions
    WHERE user_id IS NOT NULL
    GROUP BY user_id
)
SELECT s.user_id                                                   AS 계정,
       u.email                                                     AS 이메일,
       u.display_name                                              AS 이름,
       s.sessions                                                  AS 접속,
       ROUND(s.connected_hours::numeric, 1)                        AS 접속_시간h,
       COUNT(r.session_id)                                         AS 방송_회차,
       COUNT(r.session_id) FILTER (WHERE r.on_air_seconds >= 300)  AS 방송_5분이상,
       ROUND((SUM(r.on_air_seconds) / 3600)::numeric, 1)           AS 방송_시간h,
       ROUND((AVG(r.on_air_seconds) / 60)::numeric, 1)             AS 평균_방송_분,
       MIN(r.round_start)::date                                    AS 첫_방송,
       MAX(r.round_start)::date                                    AS 마지막_방송,
       (SELECT ROUND(AVG(gap_hours)::numeric, 1) FROM gaps g WHERE g.user_id = s.user_id)
                                                                   AS 평균_간격h,
       COUNT(DISTINCT DATE_TRUNC('week', r.round_start))           AS 방송한_주
FROM sessions s
LEFT JOIN users u ON u.id = s.user_id
LEFT JOIN usage_rounds r ON r.user_id = s.user_id
GROUP BY s.user_id, u.email, u.display_name, s.sessions, s.connected_hours
ORDER BY 방송_회차 DESC, 접속 DESC;

-- 3. 회차별 방송 간격 -----------------------------------------------------
\echo '== 3. 회차별 (계정, 시작순) — 직전 방송 이후 몇 시간 만인가'
SELECT user_id                                                     AS 계정,
       round_start                                                 AS 시작,
       ROUND((on_air_seconds / 60)::numeric, 1)                    AS 방송_분,
       platforms                                                   AS 플랫폼,
       ROUND((EXTRACT(EPOCH FROM round_start
             - LAG(round_end) OVER (PARTITION BY user_id ORDER BY round_start)) / 3600)::numeric, 1)
                                                                   AS 직전_이후h
FROM usage_rounds
WHERE user_id IS NOT NULL
ORDER BY user_id, round_start;

-- 4. 주간 활성 스트리머 ---------------------------------------------------
\echo '== 4. 주별 방송한 계정 (신규 / 재방문)'
WITH weekly AS (
    SELECT DISTINCT user_id, DATE_TRUNC('week', round_start)::date AS week
    FROM usage_rounds
    WHERE user_id IS NOT NULL
),
first_week AS (
    SELECT user_id, MIN(week) AS first_week FROM weekly GROUP BY user_id
)
SELECT w.week                                          AS 주,
       COUNT(*)                                        AS 방송한_계정,
       COUNT(*) FILTER (WHERE w.week = f.first_week)   AS 신규,
       COUNT(*) FILTER (WHERE w.week > f.first_week)   AS 재방문
FROM weekly w
JOIN first_week f USING (user_id)
GROUP BY w.week
ORDER BY w.week;

-- 5. 플랫폼 · 동시 송출 ---------------------------------------------------
\echo '== 5. 플랫폼 조합별 회차'
SELECT platforms                                       AS 플랫폼,
       COUNT(*)                                        AS 회차,
       ROUND((SUM(on_air_seconds) / 3600)::numeric, 1) AS 방송_시간h
FROM usage_rounds
GROUP BY platforms
ORDER BY 회차 DESC;

-- 6. 종료 사유 -------------------------------------------------------------
\echo '== 6. 송출 종료 사유 (사용자 종료 vs 장애)'
SELECT COALESCE(end_reason, '(열림)') AS 사유,
       COUNT(*)                        AS 송출,
       COUNT(*) FILTER (WHERE ended_at_estimated) AS 추정
FROM usage_broadcasts
GROUP BY end_reason
ORDER BY 송출 DESC;
