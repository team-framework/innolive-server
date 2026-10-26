# VOD 작업 서버 구현과 검증

## 계약과 적용 범위

- 이슈 #521, 1단계 계약 커밋 `6b8f58e`의 `api/vod/openapi.yaml` 기준 구현
- 경로 `/vod/jobs`, `/vod/jobs/{job_id}`, `/vod/jobs/{job_id}/result`, snake_case JSON 사용; `/api/v1` 별칭 미등록
- 확정 인터페이스 `/workspace/innolive-vod/interface-A.md`(innolive-ai `f0c24dd`, innolive-server `d3d1fa3` 기준)의 `VodProcessor` 서비스와 필드 번호 반영
- 기존 `AiProcessor` 연결·화이트리스트 export·인증·사용자 작업 게이트 재사용
- 워커 취소 상태 `cancelled` 추가; DELETE는 취소 의무를 DB에 기록하고 목록·상세·다운로드에서 즉시 숨김
- 계획서 `/workspace/innolive-video/plan.md` §3.2-0, §3.2-1, §3.5의 기존 워커 재사용·로컬 공유 디스크·라이브 우선 원칙 반영
- 업로드·목록·상세 응답에 선택 문자열 `original_filename` 추가; DB NULL은 JSON 필드 생략, 기존 작업도 생략
- multipart `file`의 filename에서 `/`·`\` 기준 마지막 요소만 보존; Unicode 제어문자와 NUL 제거, 잘못된 UTF-8은 U+FFFD 치환, 앞뒤 공백 정리 후 UTF-8 경계에서 255바이트로 자르고 끝 공백 재정리
- 정리 결과가 빈 값·`.`·`..`이면 미저장; 저장 경로는 UUID 기반으로 유지, 다운로드 `Content-Disposition`은 `vod-<id>.mp4` 유지
- 작업 큐는 PostgreSQL 테이블과 advisory lock 사용; 게스트 입장 대기열 및 Redis 데이터와 분리

## 설정

| 환경 변수 | 기본값 | 의미 |
| --- | --- | --- |
| `VOD_ENABLED` | `false` | 라우트·DB 연결·스케줄러·스위퍼 생성 여부 |
| `VOD_ROOT` | `/var/lib/innolive/vod` | 서버와 워커의 동일한 절대 공유 경로 |
| `VOD_MAX_UPLOAD_BYTES` | `2147483648` | 파일당 2 GiB, multipart 전체는 추가 1 MiB 허용 |
| `VOD_MAX_DURATION_SEC` | `3600` | 영상당 60분 |
| `VOD_MAX_CONCURRENCY` | `1` | 전체 VOD 동시성 상한 |
| `VOD_WORKER_GPUS` | 빈 값 | 쉼표로 구분한 `host:port=GPU-UUID` 매핑 |
| `VOD_MIN_FREE_MIB` | `4096` | GPU 여유 메모리 하한 |
| `VOD_RESULT_RETENTION_SEC` | `604800` | 완료 후 결과 보관 7일 |
| `VOD_ORIGINAL_RETENTION_SEC` | `86400` | 종료 후 원본 파기 상한, 0..86400 |
| `VOD_SWEEP_INTERVAL` | `1m` | 정리 재시도·고아 업로드 탐색 주기 |
| `VOD_UPLOAD_TIMEOUT` | `30m` | 업로드 읽기 및 미디어 검사 기한 |

- 활성화 시 `VOD_MAX_UPLOAD_BYTES > 2147483648` 또는 `VOD_MAX_DURATION_SEC > 3600`은 시작 오류; 더 작은 양의 상한은 허용
- 워커의 고정 상한과 일치하도록 기본값 유지; 설정을 조용히 낮추면 지정값과 실제 허용량이 달라지므로 시작 시 명시적 거부 선택
- 파일 바이트 상한까지 저장 후 초과 판별용 1바이트만 읽고 즉시 중단; 초과 바이트는 파일에 기록하지 않으며 multipart 파서의 제한된 read-ahead 외 남은 본문 미소비
- GPU당 VOD 1건은 고정 정책; 같은 GPU의 다른 워커에 라이브가 있어도 VOD 배정 금지
- 모든 현재 풀 타깃의 GPU UUID 매핑 필요; 누락된 교체 슬롯이나 GPU 메모리 조회 실패 시 VOD 배정 중단
- `nvidia-smi --query-gpu=uuid,memory.free --format=csv,noheader,nounits`로 실제 여유 메모리 조회
- `ListSessions.active_stream_count`로 다른 서버 슬롯의 라이브 스트림도 확인; 로컬 라이브 배정은 풀 잠금 안에서 채널만 닫고 즉시 반환
- 별도 양보 제어 고루틴에서 Pause RPC 호출; Start·DB·메모리 조회 지연이 Pause 전달을 막지 않도록 분리
- 기본 300프레임 세그먼트의 완료 수로 API 진행률 산출; 재개 시 진행률 감소 방지, DONE 전 최대 99
- 전체 대기열 순번과 최근 처리 FPS 기반 ETA 계산; 처리 속도나 라이브 대기 시간을 예측할 수 없으면 null 반환
- 체크포인트 토큰·프레임·세그먼트 위치 DB 보존; 재개 시 `resume=true`와 같은 토큰, 다시 export한 whitelist 전달
- 생체 임베딩의 DB·파일 저장 없음; API 커서 HMAC 키는 프로세스 메모리에만 생성하므로 재시작/서버 슬롯 전환 시 기존 커서가 400으로 무효화될 수 있음

## 보존과 장애 처리

- 완료·실패·취소가 확인되면 원본·중간 파일을 다음 정리 실행에서 조기 파기; 24시간은 최소 보관 기간이 아닌 상한
- 취소 RPC 응답이 없으면 파기 시한까지 재시도; 시한에는 서버 소유 파일 경로를 제거하고 `vod_retention_worker_unconfirmed` 기록
- 만료 시각에 맞춘 타이머와 주기적 재시도 병행; 다운로드는 스위퍼와 무관하게 만료 시 차단
- 다운로드 중 매 청크에서 소유 작업의 유효 상태 재확인; 별도 감시로 느린 클라이언트의 막힌 쓰기도 중단
- 삭제마다 작업 UUID·파일 종류·상대경로 SHA-256·삭제 시각·사유를 `vod_deletion_logs`와 구조화 로그에 기록; 파일 제거 후 DB 기록 전에 중단된 경우 재시도 시 보완
- `original_filename`은 원본 파일 조기 파기 후에도 결과 보존기간(기본 7일, `VOD_RESULT_RETENTION_SEC`)까지 보존; 만료 시 결과 파기 스위퍼가 NULL로 제거하고 만료 응답은 즉시 필드 생략
- 삭제·취소·실패 시 파일명 즉시 NULL 처리; 계정 탈퇴 트랜잭션도 VOD 활성화 여부와 무관하게 남는 행의 파일명을 NULL로 처리하고 버전 증가로 늦은 워커 저장 차단
- 파일명 파기는 워커 응답·파일 제거 성공을 기다리지 않으며 DB 장애·버전 충돌 시 스위퍼에서 재시도; `vod_deletion_logs`와 애플리케이션·SQL 로그에 원본 파일명 미기록
- 삭제 로그의 동일 작업·종류·경로 중복 기록 방지; 기존 파일 원문이나 임베딩 로그 출력 없음
- 작업 행 없이 남은 UUID 업로드 디렉터리는 업로드 제한 시간+스위퍼 주기 경과 후 재조회하여 정리
- `os.OpenRoot`로 결과 다운로드·파기 경로를 공유 루트 안으로 제한
- 낙관적 버전 검증으로 늦은 워커 응답의 삭제 작업 복원 차단
- PostgreSQL advisory lock으로 서버 슬롯 간 스케줄러·스위퍼 리더 1개 유지; 시작 시 List/Get/Pause/Cancel로 기존 워커 상태 대조
- 응답 없는 기존 워커가 실행 중일 가능성이 있으면 중복 실행을 피하도록 재조정 대기; API와 파기 스위퍼는 계속 동작

## 기본 비활성과 배포

- `.github/workflows/deploy.yml:3-13`: main push에서 `api/**`, `cmd/**`, `internal/**` 등의 변경이 자동 배포를 트리거
- 이 변경은 해당 경로를 포함하므로 main에 머지하면 배포 대상에 해당; 이번 작업에서 push·PR 변경·머지·운영 접속 미수행
- `internal/server/vod.go:22`의 라우트 조립과 `:30`의 백그라운드 시작 모두 `VOD_ENABLED` 확인 후 실행
- 비활성 시 VOD 라우트 미등록, VOD DB 연결·스케줄러·스위퍼·양보 고루틴 생성 없음
- 기존 `RunLimitEnforcer` 컨텍스트 및 `WaitGuestCleanup` 종료 훅에 VOD 생명주기 연결; PR #519의 main·배포·세션 이관 파일 수정 없음
- 마이그레이션은 플래그와 별개로 추가 전용 테이블·인덱스·nullable 컬럼 생성; 기존 auto/versioned/off 방식 유지

## 활성화 전 필요한 검증과 한계

- 실제 innolive-ai VOD 워커·NVIDIA GPU E2E는 미실행; gRPC 가짜 서버와 가짜 워커로 계약·제어 흐름 검증
- 워커의 동일 proto 배포, 공유 루트 마운트, 서버 UID 65532와 워커의 읽기/쓰기 권한 정합 확인 필요; 현재 작업 디렉터리 0750, 입력 파일 0640 사용
- 서버 컨테이너의 `nvidia-smi`와 NVIDIA utility 접근, 모든 현재/교체 슬롯의 정확한 GPU UUID 매핑 확인 필요
- 갑작스러운 워커 종료·풀에서 제거된 옛 주소의 응답 불능 시 자동 재조정이 대기할 수 있음; 옛 워커 정지 확인과 타깃/매핑 복구 필요
- 체크포인트 직후 Watch 응답 유실 시 마지막으로 DB에 저장한 토큰과 파일의 토큰 불일치 가능; 워커의 CHECKPOINT_MISMATCH는 실패로 보존하며 임의 초기화하지 않음
- 서버/DB/파일시스템 장기 장애나 워커가 이미 연 파일 디스크립터를 유지하는 상황에서 물리 파기 시각의 절대 보장 불가; 파기 실패 및 미확인 취소 로그에 대한 운영 감시 필요
- 계정 탈퇴 시 원본 파일명은 파기하지만 모든 VOD 작업을 일괄 취소하는 연결, 장기 대기 작업 최대 수명, 사용자별 저장 용량 제한은 이번 범위에 없음
- 목록/스케줄링은 현재 전체 작업 스냅샷 사용; 대규모 누적 작업의 SQL 페이지네이션·메타데이터 보존 최적화 필요

## 롤백

- 먼저 VOD를 비활성화하고 실행 중인 작업의 Pause/Cancel 및 서버 종료 완료 확인
- 전체 VOD 롤백 시 원본·결과·중간 파일 정리 및 파기 로그 보존 완료 후 아래 down 파일 적용
- 원본 파일명 기능만 롤백: 서버 중지 후 `internal/database/migration/sql/000019_add_vod_original_filename.down.sql` 적용(`ALTER TABLE vod_jobs DROP COLUMN IF EXISTS original_filename;`), 이전 VOD 바이너리로 복귀
- versioned 모드의 기능 롤백은 19→18 한 단계 down; 작업 행·결과·감사 로그는 보존하지만 원본 파일명은 복구 불가
- auto 모드도 동일 down SQL 적용 후 이전 바이너리로 복귀; 새 바이너리 재기동 시 nullable 컬럼 재생성
- 전체 VOD 롤백은 `000019_add_vod_original_filename.down.sql` → `internal/database/migration/sql/000018_create_vod_jobs.down.sql` 순서 적용

```sql
DROP TABLE IF EXISTS vod_deletion_logs;
DROP TABLE IF EXISTS vod_jobs;
```

- 두 VOD 테이블의 인덱스는 테이블과 함께 제거; VOD 외 기존 테이블·인덱스에 대한 DROP/ALTER 없음
- versioned 모드의 전체 롤백은 기존 migrate 도구로 19→18→17 두 단계 down 적용하여 `schema_migrations` 버전도 동기화
- auto 모드에서는 위 SQL 적용 후 VOD가 포함되지 않은 이전 바이너리로 복귀; 새 바이너리의 auto 마이그레이션은 재기동 시 두 테이블을 다시 생성
- 파일 데이터는 SQL down으로 삭제되지 않으므로 반드시 파일 정리 선행
- 동일 SQL의 auto/versioned 일치·중복 적용·down·재적용·기존 sentinel 테이블 보존을 실제 PostgreSQL 테스트로 검증

## 기존 phase2 검증 명령과 결과

검증 환경: Go 1.25, PostgreSQL 17.11, 로컬 가짜 gRPC 워커, ffmpeg/ffprobe. 실제 운영 GPU 미사용.

```bash
# Docker CLI는 있으나 데몬 접근 불가. sudo 없이 설치된 PostgreSQL 사용.
/usr/lib/postgresql/17/bin/initdb -D /tmp/innolive-vod-pg -A trust --no-locale
/usr/lib/postgresql/17/bin/pg_ctl -D /tmp/innolive-vod-pg \
  -l /tmp/innolive-vod-pg.log -o '-h 127.0.0.1 -p 55432 -k /tmp' start
/usr/lib/postgresql/17/bin/createdb -h 127.0.0.1 -p 55432 -U box \
  -E UTF8 -T template0 vod_test
export TEST_DATABASE_URL='postgres://box@127.0.0.1:55432/vod_test?sslmode=disable'
go build ./...
go vet ./...
go test -count=1 -json ./...
go test -race -count=1 -json ./...
```

- 최초 PostgreSQL 기동: 기본 소켓 디렉터리 쓰기 권한 오류로 실패; `-k /tmp` 지정 후 정상 기동
- 최초 전체 테스트: SQL_ASCII 기본 DB의 `client_encoding=UTF8` 요구 오류로 기존 DB 테스트 34개 실패; UTF-8 전용 DB 생성 후 해결
- 초기 VOD race 테스트: Watch 고루틴의 가변 Job 읽기 경합 발견; 불변 job ID 캡처로 수정 후 재검증 통과
- 다운로드 중단 테스트: 200ms 감시만으로 빠른 전송이 DELETE 후 끝나는 결함 발견; 청크별 DB 재검증 추가 후 삭제·만료 중단 모두 통과
- protobuf 생성: Makefile의 `proto` 명령과 동일한 protoc 옵션 직접 실행(make 미설치); protoc 33.0, protoc-gen-go 1.36.12, protoc-gen-go-grpc 1.6.2 사용

최종 명령별 집계는 아래 표에 기록한다. 테스트 수는 하위 테스트를 포함하는 Go JSON pass/skip 이벤트 수이며 테스트 없는 5개 패키지는 건너뜀 수에서 제외한다.

| 명령 | 결과 |
| --- | --- |
| `go build ./...` | 통과 |
| `go vet ./...` | 통과 |
| `go test -count=1 -json ./...` | 1,301 통과 / 0 실패 / 1 건너뜀; 테스트 패키지 15개 통과 |
| `go test -race -count=1 -json ./...` | 1,301 통과 / 0 실패 / 1 건너뜀; race 0건; 테스트 패키지 15개 통과 |

- 유일한 건너뜀: 기존 `TestLocalDeviceMediaProbe`, 명시적 장치 probe 실행 opt-in 미설정
- 라이브 회귀: `TestVODLiveAcquireRemainsImmediate`, `TestVODGPUAdmissionAndLiveMovePreemption`, `TestSchedulerLivePreemptionCheckpointResume`, `TestSchedulerObservesLiveFromOtherServerSlot`, `TestVODUnmappedReplacementStopsAdmission`
- 플래그 검증: `TestVODDisabledNoRoutesOrBackgroundLifecycle`, `TestDisabledCreatesNoRoutesOrWorkers`
- 파기 경계: `TestRetention24HoursAndSevenDays`, `TestCancellationDeadlinePurgesEvenWhenWorkerUnavailable`
- 동시성·복구: `TestSchedulerGlobalAndGPUConcurrency`, `TestSchedulerPostgresLeaderExclusive`, `TestSchedulerReconcilesWorkerRestart`
- 실제 DB 및 API: 본인 작업·타인 404·로그인 필수·크기/길이 경계·미디어 검증·커서 변조·삭제 후 응답·활성 다운로드 중단·파기 감사·마이그레이션 롤백 검증

- 최종 `internal/ai` 79개 및 `internal/vod` 38개 테스트 이벤트 모두 일반·race 실행 통과
- `git diff --check` 통과; PR #519 파일 목록과 변경 경로의 교집합 없음


## 원본 파일명 추가 검증

- 환경: Go 1.27.3, 로컬 PostgreSQL 17.11, UTF-8 `vod_test` DB, 가짜 gRPC 워커 및 ffmpeg/ffprobe; 운영 서버·GPU 미사용
- 기존 phase2의 `/tmp/innolive-vod-pg` 클러스터를 사용하되 `55432` 포트 충돌로 `55433` 포트에서 기동

```bash
/usr/lib/postgresql/17/bin/pg_ctl -D /tmp/innolive-vod-pg \
  -l /tmp/innolive-vod-filename-pg.log -o '-h 127.0.0.1 -p 55433 -k /tmp' start
export TEST_DATABASE_URL='postgres://box@127.0.0.1:55433/vod_test?sslmode=disable'
go build ./...
go vet ./...
go test -count=1 ./internal/vod ./internal/auth
go test -race -count=1 -json ./...
```

| 검증 | 결과 |
| --- | --- |
| `go build ./...` | 통과 |
| `go vet ./...` | 통과 |
| VOD·인증 집중 테스트 | 2개 패키지 통과 |
| `go test -race -count=1 -json ./...` | 1,343 통과 / 0 실패 / 1 건너뜀; race 0건; 테스트 패키지 15개 통과 |
| OpenAPI | YAML 파싱·선택 문자열 스키마·TypeScript 선택 필드 확인 |

- 테스트 수는 하위 테스트 포함 Go JSON 이벤트 기준; 테스트 없는 5개 패키지는 테스트 건너뜀 수에서 제외
- 건너뜀: 기존 `TestLocalDeviceMediaProbe`, 명시적 장치 probe opt-in 미설정
- 추가 테스트 이벤트 42개 통과; `internal/vod` 전체 80개 통과
- 정리: 경로·역슬래시·혼합 경로·제어문자·NUL·공백·잘못된 UTF-8·255바이트 경계·한글/이모지 중간 절단·빈 값·점·끝 구분자 확인
- API 및 개인정보: 업로드·목록·상세의 포함/생략, 다운로드 파일명 유지, 기본 7일/사용자 지정 보존기간 경계, 만료 전 원본 조기 파기 후 이름 보존, 만료 응답 즉시 생략, SQL NULL 및 결과 파일 제거 확인
- 파기: 삭제·취소·실패 및 계정 탈퇴(VOD 비활성 포함), 탈퇴 실패 롤백, 다른 사용자 보존, 늦은 작업 저장 충돌, 파일 파기 실패 시에도 이름 제거, 구조화/SQL/파기 감사 로그 미노출 확인
- 마이그레이션: 000019 up/down 반복 적용, 작업 행 보존, 기존 행 NULL, auto/versioned SQL 일치, auto 재적용 확인; 기존 마이그레이션 수정 없음
- `git diff --check` 통과; PR #519 변경 파일과 교집합 없음; 원격 쓰기 미수행
