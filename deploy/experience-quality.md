# 웹 체험 품질 이벤트

대상은 landing 웹 체험의 품질 측정이다. 방송·회원 API와 signaling 필드는 유지한다.
수집 API는 additive 변경이며 기존 클라이언트는 호출하지 않아도 동작한다.

## HTTP 계약 v1

- `POST /experience-quality`, JSON, 최대 1024 bytes.
- Next 서버만 `Authorization: Bearer <EXPERIENCE_QUALITY_INGEST_KEY>`로 호출한다.
  키는 32자 이상 랜덤 값으로 양쪽 runtime에 주입한다. 사용자 인증 토큰을 사용하지 않는다.
  키 미설정 시 라우트가 등록되지 않는다. 브라우저에 키를 노출하지 않는다.
- 필수 필드: `version=1`, `attemptId` UUID v4, `event`, `role`, `locale`,
  `retry` boolean, `stage`, `elapsedMs` 정수 0~86400000.
- event: started, transport_connected, first_frame, failed, ended, cancelled.
- stage: session, queue, camera, signaling, transport, first_frame, streaming.
- role: member, guest. locale: ko, en, ja.
- failed는 code가 필수이며 permission_denied, camera_missing, timeout,
  request_failed, connection_failed 중 하나다. 다른 event에는 code를 넣지 않는다.
- 선택 필드 browser: safari, chrome, firefox, edge, other, unknown.
  release: 영문·숫자·점·밑줄·하이픈 1~64자. 미전송 시 둘 다 unknown.
  release는 Next의 `INNOLIVE_WEB_REVISION` runtime 값이다.
- 미등록 필드·원본 User-Agent·회원 ID·세션 ID·토큰·쿠키·영상·IP를 받지 않는다.
  `attemptId`는 체험 시도마다 새로 생성하며 계정·방송 테이블과 연결하지 않는다.
- DB 저장 완료 또는 기존 동일 이벤트 확인 후 204(no-store).
  인증 오류 401, 잘못된 payload 400, 크기 초과 413, Content-Type 오류 415,
  수집량 제한 429(Retry-After 60), DB 오류·timeout 503. 응답에 원본 오류를 넣지 않는다.
- 인증된 요청은 프로세스당 분당 1200건 제한. IP·회원 식별자를 제한 키로 저장하지 않는다.
  다중 인스턴스에서는 인스턴스별 제한이며 중앙 제한은 아니다.

## 저장과 보관

`experience_quality_events`의 기본 키는 `(attempt_id,event)`다.
중복은 ON CONFLICT DO NOTHING으로 처리하며 최초 값·received_at을 바꾸지 않는다.
received_at은 서버 시각이다. 재배포 후에도 기존 PostgreSQL에 남는다.

versioned migration은 000013, auto migration은 동일 schema.sql을 사용한다.
DATABASE_MIGRATION_MODE=off면 먼저 마이그레이션을 별도로 적용해야 한다.
이 테이블 생성은 기존 테이블을 변경하지 않으며 구버전 서버로 롤백해도 보존된다.

기동 시 및 매시간 30일 초과 행을 삭제한다. 1000행씩 최대 100배치, sweep당 5초 제한이다.
정상 기동 중에는 보통 30일+1시간 이내 삭제하지만 장애·큰 backlog 시 지연될 수 있다.
수집 키를 해제해도 보관 정리는 계속 실행한다. DB 백업·접속 로그의 보관은 별도 정책이다.
원본 이벤트와 인증 헤더를 애플리케이션 로그에 출력하지 않는다.

## 배포

서버의 `/etc/innolive/server-secrets.env`와 landing의 보안 runtime env에 동일 수집 키를 추가한다.
landing에는 `EXPERIENCE_QUALITY_SERVER_URL`도 지정한다. HTTPS 또는 loopback HTTP만 허용한다.
NEXT_PUBLIC·Docker build ARG·GitHub 출력에 수집 키를 넣지 않는다.
서버 배포와 migration, 수집 키 설정을 먼저 적용한 뒤 웹 전달 기능을 배포한다.
미설정·저장 실패는 웹 수집 API가 503을 반환하지만 체험은 계속 동작한다.

## 조회와 검증

허가된 DB 연결에서 `psql`의 `-f scripts/experience-quality/report.sql`로 최근 7일 집계를 확인한다.
브라우저·릴리즈별 첫 프레임 성공률, 재시도 성공률, 연결·첫 프레임 p50/p95,
실패 단계, open·orphan 시도를 출력한다. started가 있는 시도만 성공률 분모에 넣는다.
집계는 클라이언트 측정값이며 유실·위조가 가능하다. 과금·사용량 판단에는 사용하지 않는다.
지속적인 영상 정지나 비식별화 효과 자체를 측정하지 않는다.

`TEST_DATABASE_URL`로 격리된 PostgreSQL 테스트 schema를 생성해 중복·HTTP 저장·보관·versioned
migration을 검증한다. 테스트 schema는 종료 시 정리한다. 운영 DB 주소를 사용하지 않는다.
