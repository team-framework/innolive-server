# VOD 운영 배포 런북

기준: `feat/vod-jobs/#521`, PR #522, 서버 소스 `b7d5d1b`. 아래 `파일:라인`은 이 소스 기준이며, 절대 경로는 별도 워커 인터페이스 문서다. 명령은 운영 담당자가 실행할 절차 예시이고 이 문서 작성 과정에서 실행한 배포 기록이 아니다. 코드 기본값과 운영 실효값을 구분하며, 확인하지 못한 값은 **운영에서 확인 필요**로 표시한다.

## 1. 운영 env와 저장소 준비

서버 env는 `/etc/innolive/server.env`, `server-secrets.env`, `server-slot-${INNOLIVE_SLOT}.env` 순서로 주입되며 뒤의 값이 앞의 값을 덮는다. 비밀 파일 전체를 출력하지 않고 실제 컨테이너의 아래 허용된 키만 확인한다. `docker compose config`는 시크릿을 병합 출력하므로 사용하지 않는다. 근거: `deploy/compose.prod.yaml:10-11,27-31`.

| 항목 | 활성화 시 기준값 / 설정 방법 | 근거와 운영 확인 |
| --- | --- | --- |
| `VOD_ENABLED` | 최종 활성화 `true`, 최초 배포와 긴급 차단은 `false` | 기본 비활성, false면 나머지 VOD env 파싱도 생략: `internal/config/vod.go:34-50`. 라우트와 백그라운드 비등록: `internal/server/vod.go:22-32` |
| 워커 gRPC 주소 | 기존 `AI_GRPC_TARGETS`의 쉼표 구분 주소 목록 재사용. 비어 있으면 `AI_GRPC_ADDR` 하나 사용 | `internal/config/config.go:183-184,269-273,608-615`, `internal/ai/vod_client.go:5-6`. `AI_GRPC_ADDR` 코드 기본값은 `localhost:50051`이나 실제 운영 주소·포트는 **운영에서 확인 필요**. 별도 VOD 주소 env는 이 경로에 없음 |
| AI 실행 모드 | `AI_PRIVACY_MODE=real`, 현재 연결 방식은 `AI_GRPC_INSECURE=true` | real일 때만 AI pool 생성, TLS 설정 false는 시작 오류: `cmd/server/main.go:138-150`. 실제 운영 설정은 **운영에서 확인 필요** |
| `VOD_WORKER_GPUS` | `<실제 target>=<실제 GPU UUID>,<다른 target>=<실제 GPU UUID>` | 쉼표 구분 `target=GPU-UUID`: `internal/config/vod.go:95-100`, `internal/config/config.go:608-615`. 같은 물리 GPU의 모든 슬롯은 같은 UUID, pool의 모든 target과 교체 슬롯까지 매핑 필요. 누락 시 VOD 배정 중단: `internal/ai/vod.go:28-55`, `internal/vod/scheduler.go:525-535`. 실제 매핑은 **운영에서 확인 필요** |
| `VOD_ROOT` | `/var/lib/innolive-ai/vod`를 서버 `server.env`와 AI 워커(`VOD_ROOT`/`--vod-root`) 양쪽에 **명시** | 서버 코드 기본값은 `/var/lib/innolive/vod`(`internal/config/vod.go:35`)라 env를 빼면 경로가 어긋남. AI 워커 유닛이 `ProtectSystem=strict`, `ReadWritePaths=/var/lib/innolive-ai`라 `/var/lib/innolive` 아래에는 쓸 수 없어 공유 경로를 워커 쓰기 가능 경로로 통일(InnoLive PM 결정, 유닛 설정은 운영에서 확인 필요). 서버 컨테이너는 같은 절대 경로로 마운트: `deploy/compose.prod.yaml`의 `/var/lib/innolive-ai/vod:/var/lib/innolive-ai/vod`. 레이아웃 근거: `/workspace/innolive-vod/interface-A.md:285-297` |
| 소유자·권한 | 디렉터리 소유 `innolive-ai`, POSIX ACL로 서버 UID `65532`와 `innolive-ai`에 `rwx`, 같은 내용의 default ACL 설정(아래 (b) 명령) | 공유 그룹+setgid 방식은 워커 umask(보통 022)가 그룹 쓰기를 지워 상대편이 하위 파일을 지울 수 없게 되므로 제외. default ACL이 있으면 새 파일·디렉터리에 umask 대신 ACL이 상속됨. 서버 UID: `Dockerfile:13-17`. **주의:** 서버는 작업 디렉터리를 `0750`으로 만든다(`internal/vod/http.go:137`). 생성 모드가 ACL mask를 `r-x`로 제한해 워커가 그 안에 `work/`·`output.mp4`를 만들 수 없으므로, 기능을 켜기 전에 `0770`으로 바꾸는 코드 수정이 필요(이 PR 범위 밖 후속, 꺼진 상태 머지에는 영향 없음). 원본 파일 `0640`은 워커가 읽기만 하므로 그대로 둠 |
| `VOD_MAX_DURATION_SEC` | `3600`초 | `internal/config/vod.go:12-16,68-75,109-120`. 더 작은 양수 허용, 3600 초과는 VOD 활성화 시 서버 시작 오류 |
| `VOD_MAX_UPLOAD_BYTES` | `2147483648` B = 2,147,483,648 B = 2 GiB | `internal/config/vod.go:12-16,56-63,109-120`. 더 작은 양수 허용, 워커 고정 상한 초과는 서버 시작 오류. 설정 오류 전파: `internal/config/config.go:252-255`, `cmd/server/main.go:34-37` |
| `VOD_RESULT_RETENTION_SEC` | `604800`초 = 7일 | `internal/config/vod.go:35-37,68-75`, `internal/vod/service.go:133-138`. 성공 완료 시 결과 만료 시각 확정 |
| `VOD_ORIGINAL_RETENTION_SEC` | `86400`초 = 24시간, 허용 범위 0..86400 | `internal/config/vod.go:36,68-75,119`, `internal/vod/service.go:121-138`. 종료 후 파기 상한이며 보장 보관 시간이 아님. 종료 확인 후 원본·work·partial을 먼저 정리: `internal/vod/sweeper.go:14-16,43-69` |
| `VOD_SWEEP_INTERVAL` | `1m` | 기본 1분, Go duration 형식, `0 < 값 <= 1h`: `internal/config/vod.go:37,77-87,119`. 다음 기한이 더 가까우면 대기를 단축하고 삭제 시 깨움: `internal/vod/sweeper.go:124-158`, `internal/vod/http.go:332-335` |
| `VOD_MIN_FREE_MIB` | `4096` MiB | GPU 여유 메모리 기준이며 디스크 여유 공간 기준이 아님: `internal/config/vod.go:36,56`, `internal/vod/worker.go:120-137`, `internal/ai/vod.go:43-46`. 실제 GPU 여유량·조정값은 **운영에서 확인 필요** |
| `VOD_MAX_CONCURRENCY` | `1` | 양의 정수, 전체 VOD lease 수 제한과 GPU당 1개 제한: `internal/config/vod.go:36,89-93,119`, `internal/ai/vod.go:36-45`. 증설 시 실제 용량은 **운영에서 확인 필요** |
| `VOD_UPLOAD_TIMEOUT` | `30m` | Go duration 형식, 양수: `internal/config/vod.go:37,77-87,119`. 요청 context와 업로드 read deadline에 적용: `internal/vod/http.go:90-94` |
| 리버스 프록시 업로드 제한 | 전체 multipart를 최소 `2148532224` B까지 전달할 수 있는지 확인 | 서버 본문 상한은 파일 상한 + `1048576` B: `internal/vod/http.go:95-103`. 실제 프록시/CDN의 본문 제한·읽기/응답 timeout·라우팅은 **운영에서 확인 필요**. Caddy 설정 경로는 레포 밖 운영 env에서 읽음: `deploy/apply-release.sh:26-30` |
| `DATABASE_MIGRATION_MODE` | 현재 운영 모드를 유지하며 아래 절차로 확인 | 코드 기본 `auto`: `internal/config/config.go:247-249`. 실제 값은 **운영에서 확인 필요**. 버전 19 확인을 위해 임의로 `auto`를 `versioned`로 바꾸지 않음 |

공유 디렉터리는 서버가 작업별 디렉터리를 `0750`, 입력 파일을 `0640`으로 생성한다. 따라서 루트 디렉터리의 그룹 쓰기 권한만 추가해도 워커가 새 작업 디렉터리에 쓸 수 있는 것은 아니다. 동일 UID 실행 또는 생성 후 유효 권한까지 보장하는 운영 권한 구성이 필요하다. 서버가 워커 생성 결과를 읽고 work 하위 파일을 삭제할 수 있는지도 확인한다. 특정 워커 계정이나 GID, 일괄 `chmod` 값을 가정하지 않는다. 근거: `internal/vod/http.go:135-149`, `internal/vod/http.go:371-384`, `internal/vod/sweeper.go:100-106`, `/workspace/innolive-vod/interface-A.md:297`.

워커 한도 env 이름은 서버와 다르다. 인터페이스의 워커 기본값은 `VOD_MAX_INPUT_BYTES=2147483648`, `VOD_MAX_DURATION_SECONDS=3600`, `VOD_MAX_JOBS=1`이며 마지막 값이 0이면 RPC가 비활성이다. 실제 워커 설정이 서버와 일치하는지는 **운영에서 확인 필요**. 근거: `/workspace/innolive-vod/interface-A.md:372-381`.

## 2. 배포 경로와 마이그레이션

main push의 변경 경로가 `api/**`, `cmd/**`, `internal/**`, `go.mod`, `go.sum`, `Dockerfile`, `.dockerignore` 중 하나이면 Deploy workflow 대상이다. 따라서 PR #522의 코드뿐 아니라 **이 `api/vod/DEPLOY.md`만 main에 반영해도 배포 트리거에 해당한다**. 수동 `workflow_dispatch`도 있다. 근거: `.github/workflows/deploy.yml:3-19`.

워크플로는 SHA/latest 이미지 빌드·push 후 SSH 키를 준비하고 `deploy@<운영 호스트>`에 `deploy <github.sha>`를 전달한다. 호스트·포트는 시크릿 참조이며 값은 **운영에서 확인 필요**. forced command는 `apply-release.sh`로 연결되고 이미지 pull, 로그 보관, 새 슬롯 기동·health 확인, 전환 후 옛 슬롯 정지를 수행한다. 근거: `.github/workflows/deploy.yml:44-77`, `deploy/receive-deploy.sh:10-28`, `deploy/apply-release.sh:115-148,178-195`.

**`VOD_ENABLED=false`는 재배포나 마이그레이션을 막지 않는다.** main 반영 시 워크플로의 배포 경로를 타며, 새 서버는 설정 로드와 DB 연결이 성공하면 VOD 플래그 조건 없이 `migration.Run`을 호출한다. workflow의 실제 실행 성공·환경 승인/보호 설정은 **운영에서 확인 필요**. 마이그레이션 실패 시 서버는 종료한다. 근거: `cmd/server/main.go:34-37,49-85`, `.github/workflows/deploy.yml:28-34,64-77`.

| 모드 | 시작 시 동작 | 근거 |
| --- | --- | --- |
| `versioned` | 내장 SQL에 `Up()` 적용. 이전 버전 17이면 000018, 000019를 차례로 자동 적용. 이미 최신이면 변경 없음 | `internal/database/migration/migration.go:48-49`, `internal/database/migration/versioned.go:13-31,57-63` |
| `auto` (기본) | `vod.AutoMigrate`가 `schema.sql`과 `filename.sql`을 트랜잭션에서 실행해 VOD 테이블·원본 파일명 컬럼 생성. 번호 기반 18/19 이력 적용과는 구분 | `internal/database/migration/migration.go:29-46`, `internal/vod/store.go:15-27`, `internal/vod/schema.sql:1-34`, `internal/vod/filename.sql:1` |
| `off` | `migration.Run`은 호출되지만 스키마 변경 생략. 별도 적용·검증 필요 | `internal/database/migration/migration.go:51-52` |

000018은 `vod_jobs`, 관련 인덱스, `vod_deletion_logs`를 만들고, 000019는 `vod_jobs.original_filename`을 추가한다. 근거: `internal/database/migration/sql/000018_create_vod_jobs.up.sql:1-34`, `internal/database/migration/sql/000019_add_vod_original_filename.up.sql:1`.

## 3. 기능 켜는 순서

아래 명령의 `VOD_SLOT`, `VOD_BASE_URL`, `VOD_WORKER_TARGET`, `VOD_WORKER_UNIT`, `VOD_PG_SERVICE`, `VOD_AUTH_CONFIG`, `VOD_SAMPLE`, `VOD_SMOKE_DIR`, `VOD_JOB_ID`는 운영 담당자가 확인한 값으로 설정한다. 모두 **운영에서 확인 필요**이며 임의의 주소·계정·토큰을 예시에 넣지 않는다. DB는 인증 정보를 별도 보관한 libpq service, HTTP는 권한 0600인 curl 설정 파일의 Authorization 헤더를 이용하는 절차 예시다. 도구 설치 여부와 이 인증 파일의 준비 방식도 **운영에서 확인 필요**. `set -x`, env 전체 출력, curl verbose/trace를 사용하지 않는다.

### (a) VOD_ENABLED=false로 머지·배포 후 스키마 확인

운영 담당자가 먼저 실효 `VOD_ENABLED=false`를 확인한 뒤 승인된 배포 절차로 main에 반영한다. env 파일 수정만으로 실행 중 프로세스 설정은 바뀌지 않으며 새 슬롯/프로세스에 반영되어야 한다. 근거: `internal/config/config.go:179-180,252-256`, `internal/config/vod.go:43-50`, `deploy/innolive-server@.service:25-31`.

읽기 확인 예시:

```bash
systemctl is-active innolive-server@blue.service innolive-server@green.service
docker exec "innolive-server-${VOD_SLOT}" printenv VOD_ENABLED DATABASE_MIGRATION_MODE
curl --silent --show-error --fail "${VOD_BASE_URL}/health" --output /dev/null
psql "service=${VOD_PG_SERVICE}" -X -v ON_ERROR_STOP=1 -c "SELECT to_regclass('public.vod_jobs'), to_regclass('public.vod_deletion_logs');"
psql "service=${VOD_PG_SERVICE}" -X -v ON_ERROR_STOP=1 -c "SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name='vod_jobs' AND column_name='original_filename';"
```

env 미설정 시 `printenv`가 값을 출력하지 않을 수 있으므로 표의 코드 기본값과 구분한다. `public`은 확인 쿼리 예시이며 실제 DB schema/search_path는 **운영에서 확인 필요**. `versioned`에서는 승인된 migrate 도구의 `version`으로 19 및 clean 상태를 확인한다(4절). `auto`에서는 위 객체 확인을 사용한다. health 성공만으로 VOD 스키마를 확인했다고 판단하지 않는다. 슬롯·health 근거: `deploy/apply-release.sh:61-62,99-108`; 스키마 근거: `internal/database/migration/sql/000018_create_vod_jobs.up.sql:1-34`, `internal/database/migration/sql/000019_add_vod_original_filename.up.sql:1`.

### (b) 공유 경로와 소유자·권한 준비

운영 담당자가 1절 권한 조건에 맞춰 경로를 준비한다. 호스트와 서버 컨테이너, 워커의 mount namespace에서 동일 저장소를 보는지 확인하고, 실제 샘플 작업 생성 후에도 0750/0640 권한을 다시 확인한다. 근거: `deploy/compose.prod.yaml:32-34`, `internal/vod/http.go:135-149`, `/workspace/innolive-vod/interface-A.md:285-297`.

```bash
stat -c '%u:%g %a %n' /var/lib/innolive /var/lib/innolive-ai/vod
namei -l /var/lib/innolive-ai/vod
findmnt -T /var/lib/innolive-ai/vod
df -h /var/lib/innolive-ai/vod
systemctl show "${VOD_WORKER_UNIT}" --property=User --property=Group --property=UMask
docker exec "innolive-server-${VOD_SLOT}" id
docker exec "innolive-server-${VOD_SLOT}" stat -c '%u:%g %a %n' /var/lib/innolive-ai/vod
docker exec "innolive-server-${VOD_SLOT}" sh -c 'test -r /var/lib/innolive-ai/vod && test -w /var/lib/innolive-ai/vod && test -x /var/lib/innolive-ai/vod'
```

compose 반영: 레포의 `deploy/compose.prod.yaml`에 `/var/lib/innolive-ai/vod` 마운트를 추가했지만, 운영 유닛은 `/opt/innolive/compose.prod.yaml`을 쓰고(`deploy/innolive-server@.service:30-31`) 배포 워크플로는 이 파일을 복사하지 않는다(`deploy/` 경로는 `.github/workflows/deploy.yml:3-13` paths 밖). 따라서 운영 담당자가 `/opt/innolive/compose.prod.yaml`에 같은 마운트를 수동 반영한 뒤 슬롯을 재기동해야 한다.

준비 명령(운영 담당자가 호스트에서 실행, 이 PR은 실행하지 않음). 워커 사용자 `innolive-ai`, 서버 컨테이너 UID `65532` 기준이다. 파일시스템이 POSIX ACL을 지원해야 한다(`acl` 패키지의 `setfacl`/`getfacl`).

```bash
# 1) 디렉터리 생성: 소유자는 워커, 다른 사용자 접근 없음
sudo install -d -o innolive-ai -g innolive-ai -m 0770 /var/lib/innolive-ai/vod
# 2) 서버 UID가 상위 경로를 통과할 수 있게(목록·쓰기 없이 x만)
sudo setfacl -m u:65532:--x /var/lib/innolive-ai
# 3) 공유 경로 자체 ACL
sudo setfacl -m u:65532:rwx,u:innolive-ai:rwx,m::rwx /var/lib/innolive-ai/vod
# 4) 새 하위 파일·디렉터리에 상속될 default ACL
sudo setfacl -d -m u::rwx,g::---,o::---,u:65532:rwx,u:innolive-ai:rwx,m::rwx /var/lib/innolive-ai/vod
# 5) 확인
getfacl -p /var/lib/innolive-ai /var/lib/innolive-ai/vod
```

ACL은 숫자 UID로 적용되므로 컨테이너 안 UID `65532`에 그대로 적용된다. 서버가 만든 작업 디렉터리의 실효 권한은 `getfacl -p /var/lib/innolive-ai/vod/<job_id>`의 `mask`와 `#effective`로 확인한다.

위 루트 디렉터리 확인만으로 작업 하위 디렉터리에 대한 워커 쓰기 권한까지 증명하지는 못한다. 실제 워커 사용자로 동일 경로를 읽고 쓸 수 있는지 스모크 작업의 권한과 함께 확인한다.

### (c) AI 워커의 VodProcessor 지원 확인

선행 작업 `innolive-ai#83`이 포함된 버전이 **실제 실행 중인지 운영에서 확인 필요**. PR 번호는 작업 요청에서 지정한 의존성이며, 서버 레포만으로 해당 PR의 머지·배포 상태나 실행 SHA를 확인할 수 없다. 요구 서비스는 기존 워커와 같은 gRPC 포트의 `VodProcessor`다. 근거: `/workspace/innolive-vod/interface-A.md:3-5`, `internal/ai/vod_client.go:5-6`, `api/proto/ai_processor.proto:245-261`.

서버 레포 루트에서 아래 읽기 RPC를 각 실제 target에 호출한다. proto 파일을 지정하므로 reflection 지원을 가정하지 않는다. `grpcurl` 설치·proto import 경로는 **운영에서 확인 필요**. 결과에는 작업 식별자가 포함될 수 있으므로 운영 내부에서 확인한다.

```bash
grpcurl -plaintext -import-path api/proto -proto ai_processor.proto \
  -d '{}' "${VOD_WORKER_TARGET}" VodProcessor/ListVodJobs
docker exec "innolive-server-${VOD_SLOT}" nvidia-smi --query-gpu=uuid,memory.free --format=csv,noheader,nounits
```

`UNIMPLEMENTED` 또는 비활성/준비되지 않은 응답이면 활성화하지 않는다. List 성공만으로 처리·권한 검증을 대신하지 않고 (e)의 실제 처리까지 확인한다. GPU UUID는 모든 target의 실제 GPU 배치와 대조한다. 근거: `api/proto/ai_processor.proto:260-261`, `/workspace/innolive-vod/interface-A.md:377`, `internal/vod/worker.go:120-137`, `internal/ai/vod.go:28-55`.

### (d) env 설정 후 재시작

운영 담당자가 1절 값을 설정하고 기존 슬롯 운영 절차로 서버를 재기동한다. 단일 슬롯 재시작도 라이브에 영향을 줄 수 있으므로 현재 활성 슬롯과 세션 상태를 확인한다. 여기서는 재시작을 실행하지 않는다. env 반영은 compose가 컨테이너를 기동하는 시점이며 적용 후 (a)의 health·스키마 확인과 아래 실효값 확인을 반복한다. 근거: `deploy/innolive-server@.service:25-31`, `deploy/compose.prod.yaml:27-31`, `deploy/apply-release.sh:124-148,183-193`.

```bash
docker exec "innolive-server-${VOD_SLOT}" printenv \
  VOD_ENABLED AI_PRIVACY_MODE AI_GRPC_INSECURE AI_GRPC_ADDR AI_GRPC_TARGETS \
  VOD_WORKER_GPUS VOD_ROOT VOD_MAX_UPLOAD_BYTES VOD_MAX_DURATION_SEC \
  VOD_RESULT_RETENTION_SEC VOD_ORIGINAL_RETENTION_SEC VOD_SWEEP_INTERVAL \
  VOD_MIN_FREE_MIB VOD_MAX_CONCURRENCY VOD_UPLOAD_TIMEOUT
```

### (e) 업로드·상태·다운로드·삭제와 라이브 우선 스모크

승인된 테스트 계정과 폐기 가능한 짧은 MP4/H.264, AAC 또는 무음 샘플을 사용한다. 업로드와 DELETE만 검증에 필요한 쓰기이며, 나머지는 조회한다. 공개 프록시 경로와 실제 서버 경로를 구분해 검증한다. 입력 검증 근거: `internal/vod/service.go:32-81`; 인증·경로 근거: `internal/server/vod.go:26-28`, `internal/vod/http.go:26-36,51-83`.

```bash
# 쓰기: 테스트 작업 업로드. 응답은 보호된 테스트 디렉터리에 저장.
curl --silent --show-error --config "${VOD_AUTH_CONFIG}" \
  --form "file=@${VOD_SAMPLE};type=video/mp4" \
  --output "${VOD_SMOKE_DIR}/upload.json" --write-out '%{http_code}\n' \
  "${VOD_BASE_URL}/vod/jobs"

# upload.json의 job_id를 VOD_JOB_ID에 지정한 뒤 약 5초 간격으로 확인.
curl --silent --show-error --config "${VOD_AUTH_CONFIG}" \
  "${VOD_BASE_URL}/vod/jobs/${VOD_JOB_ID}" \
  | jq '{job_id,status,progress,original_delete_at,result_expires_at,failure}'

# done 확인 후 다운로드.
curl --silent --show-error --fail --config "${VOD_AUTH_CONFIG}" \
  --output "${VOD_SMOKE_DIR}/result.mp4" --write-out '%{http_code}\n' \
  "${VOD_BASE_URL}/vod/jobs/${VOD_JOB_ID}/result"
ffprobe -v error -show_entries stream=codec_type,codec_name -of json "${VOD_SMOKE_DIR}/result.mp4"

# 쓰기: 이 스모크 작업만 삭제.
curl --silent --show-error --config "${VOD_AUTH_CONFIG}" --request DELETE \
  --output /dev/null --write-out '%{http_code}\n' "${VOD_BASE_URL}/vod/jobs/${VOD_JOB_ID}"
curl --silent --show-error --config "${VOD_AUTH_CONFIG}" \
  --output /dev/null --write-out '%{http_code}\n' "${VOD_BASE_URL}/vod/jobs/${VOD_JOB_ID}"
curl --silent --show-error --config "${VOD_AUTH_CONFIG}" \
  --output /dev/null --write-out '%{http_code}\n' "${VOD_BASE_URL}/vod/jobs/${VOD_JOB_ID}/result"
```

기대값은 업로드 201, 상태 조회 200, 완료 후 다운로드 200, 삭제 204, 삭제 후 상세·결과 404다. DELETE 204는 물리 파기 완료가 아니므로 (f)를 확인한다. 원본 파기 시각과 결과 만료 시각도 확인한다. 근거: `internal/vod/http.go:218-224,226-239,319-348,350-422`, `internal/vod/store.go:84-86`, `internal/vod/service.go:121-138`; 폴링 간격 계약: `api/vod/openapi.yaml:99-103`.

라이브 우선은 삭제 전 처리 중인 테스트 작업으로 별도 확인한다. 같은 GPU에 승인된 테스트 라이브 수요를 발생시키고 VOD가 paused/대기로 양보하며 라이브가 정상 지속되는지 관찰한다. 라이브 종료 후 자원이 비면 재개·완료되는지도 위 GET과 워커 List RPC로 확인한다. 라이브 생성 방법·허용 지연·부하 기준은 **운영에서 확인 필요**. 근거: `internal/ai/pool.go:233,280`, `internal/ai/vod.go:43-55,73-82`, `internal/vod/scheduler.go:525-559`, `/workspace/innolive-vod/interface-A.md:239-244`.

### (f) 정리 작업과 파기 로그 확인

스위퍼는 DB advisory lock을 획득한 리더에서 실행한다. 원본·work·partial은 종료 확인 후 정리하며, 결과는 만료 또는 삭제·실패·취소 때 정리한다. 워커 종료 미확인도 파기 기한을 연장하지 않고 오류 로그를 남긴다. 근거: `internal/vod/scheduler.go:34-78`, `internal/vod/sweeper.go:43-85`.

```bash
psql "service=${VOD_PG_SERVICE}" -X -v ON_ERROR_STOP=1 -v vod_job_id="${VOD_JOB_ID}" <<'SQL'
SELECT status, completed_at, original_delete_at, result_expires_at, deleted_at
FROM vod_jobs WHERE id = :'vod_job_id'::uuid;
SELECT job_id, file_kind, path_hash, deleted_at, reason
FROM vod_deletion_logs WHERE job_id = :'vod_job_id'::uuid ORDER BY deleted_at;
SQL
find "/var/lib/innolive-ai/vod/${VOD_JOB_ID}" -maxdepth 2 -type f -printf '%P\n'
journalctl -u 'innolive-server@*' --since '30 minutes ago' --no-pager \
  | rg -o 'vod_file_destroyed|vod_retention_worker_unconfirmed|vod retention retry required|vod reconciliation pending|vod scheduling deferred'
```

`VOD_JOB_ID`는 스모크 응답에서 받은 UUID인지 검증한 뒤 사용한다.

DB의 `file_kind`, 상대 경로 SHA-256인 `path_hash`, `deleted_at`, `reason`과 실제 파일 부재를 대조한다. 위 로그 명령은 비밀 정보 노출을 피하기 위해 이벤트 이름만 출력하며, 스모크 ID별 증적은 DB 조회를 사용한다. 빈 작업 디렉터리가 남는 것과 파일 파기는 구분한다. 근거: `internal/vod/service.go:108-119`, `internal/vod/sweeper.go:94-120`, `internal/vod/store.go:110-111`.

원본은 24시간을 기다리지 않고 먼저 삭제될 수 있다. 실제 7일 결과 만료와 기한 직전 다운로드 차단은 아직 도래하지 않은 시점에 검증 완료로 표시하지 않는다. 정리 실패·워커 미확인·비활성 기간의 수동 파기 담당자와 대응시간, 로그 보관 기간은 **운영에서 확인 필요**. 근거: `internal/vod/sweeper.go:14-16,46-53,124-158`, `internal/vod/http.go:355-357,393-426`.

### (g) 문제 시 즉시 끄기

운영 담당자가 `VOD_ENABLED=false`로 바꾸고 실행 중인 모든 서버 슬롯에 재기동으로 반영한다. env 파일만 변경해서는 즉시 꺼지지 않는다. (a)의 실효값 확인과 `/vod/jobs` 요청의 미등록 응답을 확인하고, 각 워커 List RPC로 남은 처리 작업을 점검한다. 종료 경로는 VOD context를 취소하고 완료를 기다리지만, 워커 통신 장애 시 종료를 단정하지 않는다. 근거: `internal/server/vod.go:22-32,60-77`, `internal/vod/scheduler.go:340-358`.

```bash
# 운영 담당자가 비활성 env를 반영해 재기동한 뒤, 살아 있는 각 슬롯에서 확인.
docker exec "innolive-server-${VOD_SLOT}" printenv VOD_ENABLED
curl --silent --show-error --config "${VOD_AUTH_CONFIG}" \
  --output /dev/null --write-out '%{http_code}\n' "${VOD_BASE_URL}/vod/jobs"
grpcurl -plaintext -import-path api/proto -proto ai_processor.proto \
  -d '{}' "${VOD_WORKER_TARGET}" VodProcessor/ListVodJobs
```

**false면 스위퍼도 멈춘다.** DB·파일·기한·감사 기록은 남으므로 기한 내 정리 책임을 운영 담당자에게 넘기고 필요하면 아래 롤백을 수행한다. 플래그를 껐다고 파일이나 스키마가 삭제되는 것은 아니다. 근거: `internal/vod/sweeper.go:17-20,124-127`, `internal/database/migration/migration.go:29-52`.

## 4. 필요 시 롤백

아래는 승인된 장애 대응 시 운영 담당자가 수행할 **변경 명령 형태**다. 문서 작성 중 실행하지 않는다. 먼저 기능 비활성화와 바이너리 복귀만으로 충분한지 판단한다. 스키마 down은 VOD 작업·감사 테이블을 잃는 별도 단계다. 000018은 기존 테이블을 바꾸지 않으므로 무조건 down부터 수행하지 않는다. 근거: `internal/database/migration/sql/000018_create_vod_jobs.down.sql:1-5`, `deploy/README.md:64,74-76`.

1. `VOD_ENABLED=false`를 모든 슬롯 env에 준비하고 서버를 중지한다. blue/green 외 실행 인스턴스도 없는지 확인한다. 워커에는 VOD Pause/Cancel 또는 승인된 워커 중지 절차를 적용하고, 파일을 다시 만들 프로세스가 없음을 확인한다. 워커 중지는 라이브도 영향을 받으므로 대상 유닛과 순서는 **운영에서 확인 필요**. 근거: `deploy/innolive-server@.service:30-33`, `internal/server/vod.go:60-77`, `api/proto/ai_processor.proto:256-261`, `internal/database/migration/sql/000018_create_vod_jobs.down.sql:1-2`.

   ```bash
   # 변경: 스키마 롤백 전에 모든 서버 슬롯 중지.
   sudo systemctl stop innolive-server@blue.service innolive-server@green.service
   systemctl is-active innolive-server@blue.service innolive-server@green.service
   ```

2. down 전에 VOD 경로의 입력·출력·work·partial을 운영 파기 절차로 정리하고 파기 증적을 확보한다. 자동 스위퍼가 이미 중지된 경우 수동 파기 증적에도 작업 ID, 파일 종류, 상대 경로 해시, 시각, 사유를 남긴다. 이미 있던 `vod_deletion_logs`와 추가 수동 증적을 접근 제한된 저장소에 반출·검증한 뒤에만 000018 down으로 진행한다. DB 백업 및 반출 파일의 보관 위치·기간·복구 책임자는 **운영에서 확인 필요**. 근거: `internal/database/migration/sql/000018_create_vod_jobs.down.sql:1-4`, `internal/vod/sweeper.go:94-119`, `internal/vod/service.go:108-119`.

   ```bash
   # VOD_AUDIT_EXPORT는 운영에서 확인한 전용 반출 파일 경로. stdout 출력 금지.
   (umask 077; psql "service=${VOD_PG_SERVICE}" -X -v ON_ERROR_STOP=1 \
     -c 'COPY (SELECT job_id,file_kind,path_hash,deleted_at,reason FROM vod_deletion_logs ORDER BY id) TO STDOUT WITH CSV HEADER' \
     > "${VOD_AUDIT_EXPORT}")
   stat -c '%a %s %n' "${VOD_AUDIT_EXPORT}"
   sha256sum "${VOD_AUDIT_EXPORT}"
   ```

   반출 명령의 성공과 내용 완전성을 내부에서 검증한다. checksum만으로 완전성을 보장하지 않는다. journald 보관도 유지한다. 레포의 로그 아카이브는 기본 `/srv/innolive-logs`를 사용하지만 실제 설치·마운트·보관 기간은 **운영에서 확인 필요**. 근거: `deploy/log-archive.sh:13-22,49-56`.

3. **versioned DB, 현재 19 및 clean 상태가 확인된 경우** 승인된 `golang-migrate` CLI로 `down 1`을 두 번 실행하고 매번 버전을 확인한다. 첫 번째는 000019 down으로 **19 → 18**, 두 번째는 000018 down으로 **18 → 17**이다. 000017 down은 적용하지 않는다. 서버는 Up만 제공하므로 이 CLI는 별도 도구이며 설치·PostgreSQL 지원·운영 접속 방법은 **운영에서 확인 필요**. 근거: `go.mod:8`, `internal/database/migration/versioned.go:8-10,57-63`, `internal/database/migration/sql/000019_add_vod_original_filename.down.sql:1`, `internal/database/migration/sql/000018_create_vod_jobs.down.sql:1-5`.

   ```bash
   # 서버 레포 루트. VOD_MIGRATION_DSN은 승인된 비밀 주입으로 준비, 출력/trace 금지.
   migrate -path internal/database/migration/sql -database "${VOD_MIGRATION_DSN}" version
   # 위 결과가 19, clean인 경우에만 변경 명령 수행.
   migrate -path internal/database/migration/sql -database "${VOD_MIGRATION_DSN}" down 1
   migrate -path internal/database/migration/sql -database "${VOD_MIGRATION_DSN}" version
   # 18, clean 확인 후 수행.
   migrate -path internal/database/migration/sql -database "${VOD_MIGRATION_DSN}" down 1
   migrate -path internal/database/migration/sql -database "${VOD_MIGRATION_DSN}" version
   # 최종 17, clean 확인.
   ```

   DSN은 프로세스 인자에 포함될 수 있으므로 승인된 관리 실행 환경에서만 사용하고 명령·오류 전문을 공개 로그에 남기지 않는다. dirty이거나 다른 버전이면 중단하여 실제 스키마와 이력을 조사한다. `force 17`로 SQL 실행을 건너뛰지 않는다. 버전·dirty 점검 도구의 동작은 설치된 CLI 도움말과 대조하며 운영에서 확인한다.

4. **auto 또는 별도 수동 적용 DB인 경우** 번호 이력을 가정하지 않는다. 앞의 중지·파기·감사 반출을 완료한 후 실제 스키마를 확인해 아래 두 SQL을 같은 순서로 적용한다. 이 방식은 스키마를 VOD 도입 전 형태로 되돌릴 뿐 versioned 이력을 17로 갱신하지 않는다. versioned DB에 이 방법을 혼용하지 않는다. 근거: `internal/vod/store.go:15-27`, `internal/database/migration/migration.go:29-52`, 두 down SQL의 위 근거.

   ```bash
   # 변경: auto/수동 적용 DB 전용. 앞 단계가 모두 완료된 경우에만 실행.
   psql "service=${VOD_PG_SERVICE}" -X -v ON_ERROR_STOP=1 --single-transaction \
     -f internal/database/migration/sql/000019_add_vod_original_filename.down.sql \
     -f internal/database/migration/sql/000018_create_vod_jobs.down.sql
   ```

5. 이전 바이너리(이전 커밋의 컨테이너 이미지)로 복귀한다. 승인된 `VOD_PREVIOUS_SHA`와 이미지 존재 여부는 **운영에서 확인 필요**. 공식 비상 경로의 명령 형태는 아래와 같다. `--force`는 세션 대기를 생략하므로 서버 중지와 영향 확인을 먼저 완료한다. 근거: `deploy/README.md:74-76`, `deploy/apply-release.sh:99-118,124-148,183-193`.

   ```bash
   # 변경: 승인된 이전 이미지로 복귀.
   sudo /opt/innolive/deploy/apply-release.sh "${VOD_PREVIOUS_SHA}" --force
   systemctl is-active innolive-server@blue.service innolive-server@green.service
   curl --silent --show-error --fail "${VOD_BASE_URL}/health" --output /dev/null
   ```

   VOD 스키마 down 뒤 새 VOD 바이너리를 다시 시작하면 `auto`/`versioned`에서 스키마를 다시 만들 수 있다. 이전 이미지 선택을 확인한 후 시작하고, health·라이브 정상 동작·VOD 비활성·파일 파기와 외부 감사 증적 보존을 최종 확인한다. 근거: `cmd/server/main.go:72-85`, `internal/database/migration/migration.go:29-52`. SQL down 자체는 디스크 파일을 삭제하거나 감사 로그를 반출하지 않는다: `internal/database/migration/sql/000018_create_vod_jobs.down.sql:1-5`.

## 5. 확인하지 못한 것

- **운영에서 확인 필요**: 실제 `AI_GRPC_TARGETS`/`AI_GRPC_ADDR`, 모든 슬롯과 물리 GPU UUID의 매핑, AI 모드·연결 설정, GPU 여유량과 동시성 조정값. 코드 계약 근거: `internal/config/config.go:183-184,269-273`, `internal/ai/vod.go:28-55`.
- **운영에서 확인 필요**: 공유 경로 실마운트, 워커 UID/GID·서버 GID·umask·ACL, 새 작업 하위 경로의 상호 읽기/쓰기/삭제 권한, 디스크 여유·용량 경보 기준. 근거: `internal/vod/http.go:135-149`, `/workspace/innolive-vod/interface-A.md:285-297`.
- **운영에서 확인 필요**: innolive-ai#83의 실제 포함 SHA와 실행 버전, `VodProcessor` RPC·워커 한도·활성화 상태. 근거: `api/proto/ai_processor.proto:245-261`, `/workspace/innolive-vod/interface-A.md:372-381`. 서버 소스만으로 원격 PR 상태·배포를 입증하지 않음.
- **운영에서 확인 필요**: 각 env의 실효값, DB migration mode·현재 버전·dirty 상태·schema/search_path, 000018/000019 적용 결과, rollback 도구·백업·이전 이미지. 근거: `internal/config/config.go:247-249`, `internal/database/migration/versioned.go:13-63`, `deploy/compose.prod.yaml:27-31`.
- **운영에서 확인 필요**: 프록시/CDN 업로드 크기·timeout·VOD 라우팅, 서비스 URL, 운영 SSH 배포 값·환경 보호 설정·실제 배포 성공 여부. 근거: `internal/vod/http.go:90-103`, `deploy/apply-release.sh:26-30`, `.github/workflows/deploy.yml:28-34,64-77`.
- **운영에서 확인 필요**: 실제 업로드·조회·다운로드·삭제, 라이브 양보·재개·허용 지연, 기한 내 파일 파기·7일 만료, 비활성 시 수동 정리 책임, 감사 로그 반출·보관 위치/기간·아카이브 설치 상태. 근거: `internal/vod/scheduler.go:525-559`, `internal/vod/sweeper.go:14-20,94-158`, `deploy/log-archive.sh:13-22,49-56`.
- 이 문서는 소스와 인터페이스 문서 대조만 수행. go test, 운영 접속(itjzb 포함), push, PR 수정, 머지, 배포, 원격 쓰기 미수행.
