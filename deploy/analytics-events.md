# 공통 웹 분석 이벤트 수집

`POST /analytics/events`는 기존 collector 서비스 키 EXPERIENCE_QUALITY_INGEST_KEY로 인증한다.
브라우저가 직접 호출하지 않으며 Next가 공개 입력을 검사한 뒤 전송한다.
서버는 이벤트별 DTO·등록 목록 없이 event 이름과 JSONB 속성을 받는다.
JSONB는 개인정보를 자동 제거하지 않는다. 신뢰한 gateway가 허용한 속성만 넣어야 한다.

공통 v1: version=1, eventId·visitId UUID v4, sequence 정수 1~1000000,
event 영문 소문자 시작 snake_case 최대 64자, properties 객체 최대 32개,
release 영문·숫자·점·밑줄·하이픈 최대64자(생략 시 unknown).
속성 키도 snake_case 최대64자, 값은 문자열128bytes 이하(제어 문자 제외)·boolean·유한 number(절댓값 2^53-1 이하)만 허용한다.
중첩 객체·배열·null은 v1에서 받지 않는다. JSON body 최대4096bytes.

인증 오류401, 본문 오류400, 크기413, JSON형식415, 분당1200건429, DB오류503.
저장 또는 동일 eventId 확인 후204. 최초 수신 시각은 중복 전송으로 변경하지 않는다.
원본 body·IP·쿠키·사용자 토큰을 애플리케이션 로그에 기록하지 않는다.
기동 및 매시간 30일 초과 기록을 1000행씩 최대100배치·5초 제한으로 삭제한다.

versioned migration 000014, auto migration 동일 schema.sql. off이면 배포 전 별도 migration 필요.
기존 API와 quality 테이블을 유지하는 additive 변경이며 rollback 시 analytics 테이블도 유지한다.
Next 배포보다 서버 배포가 먼저 필요하다. 기존 runtime collector URL·키를 재사용하므로 새 build secret이 없다.

`scripts/analytics/funnel.sql`은 최근7일 랜딩 cohort를 방문·sequence 기준으로 집계한다.
체험→가입과 직접 가입을 분리하며 같은 방문의 시간 순서를 지킨다.
전환 분석 데이터는 best effort client 관측이며 사람 수·확정 가입자 수·과금 근거로 사용하지 않는다.
