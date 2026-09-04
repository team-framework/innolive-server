# innolive-server

실시간 WebRTC 라이브 방송을 받아 **AI 비식별화** 를 거친 뒤, 처리된 영상을 사용자에게 전달하고 방송 플랫폼으로 송출하는 Go 미디어 서버입니다.

서버는 사용자의 카메라 프레임을 받아 AI 서버로 전달합니다. AI 서버는 등록된 얼굴을 제외한 나머지 얼굴을 블러 처리하고, 서버는 그 결과를 사용자와 라이브로 내보냅니다. 방송 화면에 노출된 주변 사람들의 얼굴을 가려주는 라이브 방송 서버입니다.

---

## 이 프로젝트가 해결하는 문제

야외에서 방송을 켜면 발행자 뒤로 지나가는 사람들의 얼굴이 그대로 나갑니다. innolive-server는 방송 파이프라인 중간에 AI 프라이버시 단계를 넣어 본인 얼굴만 남기고 나머지 얼굴을 블러 처리합니다. AI 처리가 실패하면 서버는 원본을 내보내는 대신 화면을 검게 덮습니다.

## 주요 기능

- **WebRTC 인제스트/전달**: [pion/webrtc](https://github.com/pion/webrtc) 기반. STUN/TURN, ICE 재연결 유예, 네트워크 전환 시 ICE restart 복구를 지원합니다.
- **AI 프라이버시 파이프라인**: 프레임을 AI 서버로 보내 얼굴을 블러 처리합니다. `bypass`, `fixed_delay`, `real` 세 모드가 있고, 워커 여러 대에 라운드로빈으로 분배합니다.
- **fail-closed 실패 처리**: AI 처리가 지연되거나 실패하면 미처리 원본 대신 검은 화면을 내보냅니다.
- **AI 워커 상시 감시**: 주기마다 실제 프레임을 왕복시키는 프로브를 돌려, 프로세스는 살아 있는데 추론만 죽은 워커를 잡아냅니다.
- **플랫폼 송출**: 사용자가 연결한 YouTube 계정으로 방송을 만들고 FFmpeg로 RTMP 송출합니다. 방송 준비와 라이브 전환을 나눠서, 시청자에게 보이기 전에 화면을 확인할 수 있습니다.
- **방송 설정 저장**: 제목·설명·공개 범위·아동용 여부·카테고리·썸네일을 세션에 저장했다가 방송 준비 때 반영합니다.
- **비식별화 토글**: 송출을 유지한 채 AI 처리만 켜고 끕니다.
- **참조 얼굴 관리**: 블러에서 제외할 "본인 얼굴"을 REST API로 등록하고 삭제합니다.
- **세션 소유권 인증**: 세션마다 발급하는 토큰으로 소유자만 세션을 제어합니다.
- **사용자 계정 관리**: PostgreSQL + GORM. Google/Apple OAuth와 이메일 인증으로 계정을 만들고, 로그인 세션과 탈퇴를 다룹니다.
- **관측성**: Prometheus 메트릭, 구조화 JSON 로그, pprof 프로파일링(기본 꺼짐).
- **번들 웹 클라이언트**: 로그인부터 방송까지 확인할 수 있는 정적 클라이언트를 `/client/`로 서빙합니다.

## 기술 스택

| 영역 | 사용 기술 |
|------|-----------|
| 언어/런타임 | Go 1.25 |
| WebRTC | pion/webrtc v4, pion/rtp |
| AI 연동 | gRPC, protobuf |
| 미디어 트랜스코딩 | FFmpeg |
| DB | PostgreSQL 16, GORM, golang-migrate |
| 캐시 | Redis 7 (이메일 인증 코드, 가입 대기 상태) |
| 플랫폼 연동 | YouTube Data API v3 (방송 생성·라이브 전환) |
| 배포 | Docker / Docker Compose, GitHub Actions |

## 아키텍처

미디어 평면은 발행자에서 뷰어와 YouTube까지 프레임이 흐르는 경로입니다. 제어 평면은 계정과 방송 설정, 플랫폼 API를 다룹니다.

```
미디어 평면

[Publisher]
    │  WebRTC (H.264 영상 + Opus 오디오)
    ▼
internal/server   시그널링(WebSocket)
    │
    ▼
internal/session  세션·소유권·ICE 복구
    │
    ▼
internal/media    Processor ──gRPC(프레임)──► [AI 서버] ──블러 처리된 프레임──┐
    │ ◄──────────────────────────────────────────────────────────────────────┘
    │
    ├──► Track  ──WebRTC──►  [Viewer]
    └──► Egress ──FFmpeg──►  RTMP  ──►  [YouTube Live]


제어 평면

internal/server (REST)
    ├──► internal/auth      ──► PostgreSQL  계정·리프레시 세션·연결된 송출 계정
    │                       ──► Redis       이메일 인증 코드·가입 대기 상태
    └──► internal/streaming ──► YouTube Data API  방송 생성 / 라이브 전환 / 종료
```

### 코드 구조

```
cmd/server/            진입점: 설정 로드, DB/마이그레이션, 서버 부트스트랩
internal/
├── config/            환경변수 기반 설정 로드/검증
├── server/            HTTP 라우팅, REST 핸들러, 시그널링, 세션 소유권 인증
│   └── static/client/ 번들 웹 클라이언트 (`/client/`로 서빙)
├── session/           세션 매니저, 소유권, 시그널링 상태, ICE 복구, 방송 설정
├── media/             WebRTC 트랙, AI 프로세서, FFmpeg, RTMP 송출, 오디오 파이프
├── ai/                AI gRPC 클라이언트 풀, 기동 preflight, 상시 준비 상태 감시
├── auth/              계정·토큰·OAuth·이메일 인증·탈퇴·연결된 송출 계정
├── streaming/         송출 플랫폼 공통 계약(Prepare/GoLive/Stop)과 YouTube 구현
├── origin/            HTTP와 WebSocket이 공유하는 단일 Origin 정책
├── database/          PostgreSQL 연결·커넥션 풀, 마이그레이션(SQL 포함)
└── metrics/           Prometheus 레지스트리, 프로세스 메트릭
api/proto/             AI 프로세서 gRPC 정의 (+ 생성된 코드)
deploy/                프로덕션 compose·릴리스 적용 스크립트·운영 가이드
```

## 시작하기

### 사전 요구사항

- Go 1.25+
- FFmpeg (모든 프라이버시 모드에서 필수)
- PostgreSQL 16 (Docker Compose로 함께 실행 가능)
- Redis 7 (이메일 회원가입과 인증 코드를 쓸 때. Docker Compose에 포함)
- (선택) AI 서버. `real` 모드에서 gRPC로 연결합니다.

PostgreSQL에 연결하지 못하면 서버는 부팅 도중 종료합니다.

### Docker Compose로 실행

```bash
cp .env.example .env
# .env에서 POSTGRES_PASSWORD 설정
docker compose up --build
```

Compose는 서버를 `:8000`(HTTP)과 `50002-50020/udp`(WebRTC)로 엽니다. UDP 포트 구성은 [WebRTC 포트 구성](#webrtc-포트-구성)을 참고하세요.

### 로컬에서 직접 실행

```bash
export DATABASE_URL="postgres://innolive:innolive@localhost:5432/innolive?sslmode=disable"
make run          # go run ./cmd/server
```

### 개발 명령어

```bash
make build        # 바이너리 빌드
make test         # 전체 테스트
make test-race    # 레이스 감지 테스트
make proto        # protobuf 코드 생성
```

`internal/media`와 `internal/session`에는 고루틴·atomic·mutex가 몰려 있습니다. 두 패키지를 고쳤다면 `make test-race`까지 돌리세요.

## 설정

모든 설정은 환경변수로 주입합니다. 전체 목록과 기본값은 [`.env.example`](.env.example)를 참고하세요. 아래 표의 기본값은 코드(`internal/config`)가 쓰는 값입니다. `.env.example`이 로컬 개발용으로 다른 값을 싣는 항목은 설명에 적었습니다.

### 서버 · AI

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `HTTP_ADDR` | `:8000` | HTTP 리스닝 주소 |
| `LOG_LEVEL` | `INFO` | `DEBUG` / `INFO` / `WARN` / `ERROR` |
| `PPROF_ENABLED` | `false` | `/debug/pprof/*` 노출. 인증이 없으니 프로파일링 중에만 켜세요 |
| `AI_PRIVACY_MODE` | `real` | `bypass` / `fixed_delay` / `real`. `.env.example`은 로컬용으로 `bypass`를 싣습니다 |
| `AI_GRPC_TARGETS` | 없음 | AI 워커 gRPC 타겟 목록(쉼표 구분). 비우면 `AI_GRPC_ADDR` 하나를 씁니다 |
| `AI_GRPC_ADDR` | `localhost:50051` | 단일 타겟 지정용 |
| `AI_GRPC_TIMEOUT` | `5s` | 프레임 한 장을 처리하는 요청의 제한 시간 |
| `AI_FAILURE_POLICY` | `blackout_latch` | AI 실패 시 정책. `blackout_latch`는 검은 화면, `freeze`는 마지막 처리 프레임 유지 |
| `AI_TIMEOUT_LATCH_THRESHOLD` | `3` | 연속 타임아웃이 이 횟수를 넘으면 블랙아웃 래치를 겁니다 |
| `AI_PREFLIGHT_REQUIRED` | `false` | 기동 preflight가 실패하면 부팅을 중단할지 여부 |
| `AI_PREFLIGHT_TIMEOUT` | `30s` | preflight 프로브 제한 시간 |
| `AI_PREFLIGHT_INTERVAL` | `5m` | 상시 감시 프로브 주기. `0`은 비활성이며 `AI_PREFLIGHT_TIMEOUT`보다 길어야 합니다 |
| `AI_FRAME_WIRE_FORMAT` | `jpeg` | `jpeg` / `raw` |
| `AI_PRIVACY_FIXED_DELAY` | `20ms` | `fixed_delay` 모드에서 넣는 지연 |

### 세션 · WebRTC

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `MAX_SESSIONS` | `0` | 동시 세션 상한. `0`은 무제한이고, 초과하면 세션 생성을 거절합니다 |
| `SESSION_NEGOTIATION_TIMEOUT` | `30s` | 시그널링을 시작하지 않은 세션을 회수하기까지의 시간 |
| `INNOLIVE_REQUIRE_SESSION_AUTH` | `true` | 사용자 인증과 세션 소유권 검사. 벤치마크에서만 끕니다 |
| `WEBRTC_STUN_URLS` | `stun:stun.l.google.com:19302` | STUN 서버 목록 |
| `WEBRTC_TURN_URLS` / `WEBRTC_TURN_USERNAME` / `WEBRTC_TURN_CREDENTIAL` | 없음 | TURN 서버와 자격증명 |
| `WEBRTC_ANNOUNCED_IP` | 없음 | 서버가 후보로 알릴 공인 IP |
| `WEBRTC_UDP_MUX_PORT` | `0` | 단일 UDP 포트로 다중화. `0`이 아니면 아래 포트 범위를 무시합니다 |
| `WEBRTC_UDP_PORT_MIN` / `WEBRTC_UDP_PORT_MAX` | `50000` / `60000` | 포트 범위 방식. `.env.example`과 Compose는 `50002` / `50020`을 씁니다 |
| `WEBRTC_DISCONNECTED_GRACE` | `10s` | `disconnected` 상태를 견디는 시간 |
| `WEBRTC_RECOVERY_WINDOW` | `50s` | 네트워크 전환 뒤 ICE restart를 기다리는 세션 유지 시간 |
| `WEBRTC_RECOVERY_DEBOUNCE` | `2s` | 일시 끊김을 거르는 시간 |
| `WEBRTC_RECOVERY_MAX_ATTEMPTS` | `10` | 서버가 수락할 ICE restart offer 상한 |

### 미디어 · 송출

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `FFMPEG_PATH` | `ffmpeg` | FFmpeg 실행 경로 |
| `FFMPEG_SPAWN_CONCURRENCY` | `3` | FFmpeg 동시 스폰 상한. 콜드 스파이크를 막는 자원 거버너입니다 |
| `FFMPEG_ENCODER_THREADS` | `1` | 인코더 스레드 수 |
| `ENABLE_AUDIO_EGRESS` | `false` | 발행자 마이크(Opus)를 함께 송출. 끄면 무음으로 나갑니다 |
| `EGRESS_AUDIO_OFFSET_MS` | `0` | A/V 싱크 보정(밀리초). 양수면 오디오를 늦춥니다 |
| `EGRESS_VIDEO_BITRATE` | 없음 | 송출 비트레이트 고정. 비우면 해상도에 따라 정합니다 |
| `EGRESS_VIDEO_SIZE` | 없음 | 송출 해상도 고정(예: `1280x720`). 비우면 첫 프레임 해상도를 따릅니다 |
| `DECODER_PIN_LONG_EDGE` | `0` | 디코더 출력 장변 고정. 켜면 GPU 부하가 크게 올라가니 `MAX_SESSIONS`를 다시 재고 프로덕션에 넣으세요 |
| `EGRESS_LATENCY_LOG` | `false` | 인제스트에서 송출까지의 지연 통계 로깅(측정용) |

> 송출 대상은 사용자마다 연결한 플랫폼 계정에서 나옵니다. 서버에 스트림 키를 넣는 설정은 없습니다.

### 데이터 · 인증

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `DATABASE_URL` | 없음 | PostgreSQL 연결 문자열. 운영에서는 TLS 연결 문자열을 쓰세요 |
| `DATABASE_MIGRATION_MODE` | `auto` | `auto`(GORM AutoMigrate) / `versioned`(내장 SQL) / `off` |
| `AUTH_ACCESS_TOKEN_TTL` / `AUTH_REFRESH_TOKEN_TTL` | `15m` / `720h` | 액세스·리프레시 토큰 수명 |
| `AUTH_CORS_ALLOWED_ORIGINS` | 로컬 개발 오리진 | 허용할 브라우저 오리진 목록 |
| `GOOGLE_OAUTH_WEB_CLIENT_ID` | 없음 | 로그인용 Google ID 토큰 audience |
| `APPLE_TEAM_ID` / `APPLE_CLIENT_ID` / `APPLE_KEY_ID` / `APPLE_PRIVATE_KEY_PATH` | 없음 | Apple 로그인 설정 |
| `YOUTUBE_OAUTH_CLIENT_ID` / `YOUTUBE_OAUTH_CLIENT_SECRET` | 없음 | **송출용** YouTube OAuth. 로그인용 Google OAuth와 별개 클라이언트이고, 둘 다 있어야 연동이 켜집니다 |
| `AUTH_PROVIDER_TOKEN_ENCRYPTION_KEY_BASE64` | 없음 | 저장하는 플랫폼 토큰을 암호화하는 32바이트 AES-256 키. 송출 연동을 켤 때 필요합니다 |
| `AUTH_EMAIL_SMTP_HOST` | 없음 | SMTP 호스트. 비어 있으면 이메일 로그인 API가 통째로 꺼집니다 |
| `AUTH_EMAIL_REDIS_ADDR` | 없음 | 가입 대기 정보와 인증 코드를 보관하는 Redis 주소 |
| `AUTH_EMAIL_VERIFICATION_CODE_TTL` | `5m` | 회원가입 인증 코드 만료 시간 |

서버는 설정이 갖춰진 기능의 라우트만 마운트합니다. SMTP 설정이 없으면 이메일 가입 경로를, YouTube OAuth 설정이 없으면 계정 연결 경로를 등록하지 않고 404로 떨어뜨립니다.

### WebRTC 포트 구성

WebRTC UDP 포트는 두 방식 중 하나로 동작하고, 로컬과 프로덕션이 서로 다릅니다.

- **포트 범위 방식**: `WEBRTC_UDP_MUX_PORT=0`일 때 `WEBRTC_UDP_PORT_MIN/MAX` 범위를 씁니다. `.env.example`과 `compose.yaml`이 이 방식입니다.
- **단일 mux 방식**: `WEBRTC_UDP_MUX_PORT`가 `0`이 아니면 그 포트 하나로 모든 세션을 다중화하고, 포트 범위 설정은 코드가 무시합니다. 프로덕션이 이 방식이니 로컬 감각으로 프로덕션 포트를 추정하지 마세요.

### WebRTC 네트워크 복구 계약

`GET /webrtc/config`은 ICE 서버 목록과 함께 아래 `recovery` 값을 반환합니다.
웹·iOS·macOS·Android 클라이언트는 이 값을 같은 복구 계약으로 사용합니다.

```json
{
  "iceServers": [{ "urls": ["turn:turn.example:3478"], "username": "…", "credential": "…" }],
  "recovery": { "window_ms": 50000, "debounce_ms": 2000, "max_attempts": 10 }
}
```

`disconnected`는 `debounce_ms` 뒤, `failed`는 즉시 같은 `RTCPeerConnection`의
ICE restart 복구 창으로 전이합니다. 각 복구 창에서는 restart offer를 **한 번만**
보냅니다. 해당 offer에는 새 UUID `negotiation_id`와 `ice_restart: true`를 넣고,
그 offer의 SDP에 있는 ICE `usernameFragment`와 일치하는 **클라이언트 local
candidate**만 같은 `negotiation_id`로 보냅니다. 이전 generation client candidate와
`candidate: null` 종료 신호는 서버에 보내지 않습니다. 서버도 `candidate: null`
종료 신호를 보내지 않습니다. 서버가 trickle로 보내는 candidate는 candidate 안의
ICE `ufrag`를 answer SDP의 local ufrag에 연결해 해당 generation의
`negotiation_id`로 전달합니다.

offer를 보낸 뒤에는 브라우저의 STUN/TURN 연결 검사가 계속 진행됩니다. 클라이언트는
5초마다 로컬 `RTCPeerConnection` 상태만 관찰하며, 그 관찰 타이머에서 추가 offer를
보내거나 `/webrtc/config`를 다시 요청하지 않습니다. 서버의 `max_attempts`는 잘못된
복구 요청을 막는 수락 상한(현재 10)이며, 정상 클라이언트의 복구 횟수 정책이 아닙니다.
`window_ms` 안에 연결되지 않으면 서버가 세션을 종료합니다. 이후 클라이언트의
`GET /sessions/{id}` polling은 `404 not_found`를 받고 PeerConnection·signaling
WebSocket·카메라·마이크를 정리합니다. `peer_connection_recovery_exhausted`는 서버
로그와 세션 close reason에서만 쓰이며 HTTP 응답 code는 아닙니다. 이전 세대 candidate는
`409 stale_negotiation`으로 거절됩니다.

복구 중 활성 RTMP egress는 사용자 pause 상태로 바뀌지 않습니다. 대신 기존 취소
슬레이트와 무음 Opus를 유지하다가, ICE 연결과 첫 정상 처리 카메라 프레임이 모두
확인된 뒤에만 카메라 영상·오디오로 돌아갑니다.

## 송출 흐름

사용자가 자기 계정을 서버에 연결합니다. 서버는 그 계정의 토큰을 암호화해 보관했다가 방송을 만들 때 씁니다.

1. **계정 연결**: 클라이언트가 얻은 인가 코드를 `POST /auth/youtube/connect`로 보내 YouTube 계정을 연결합니다. 연결 목록은 `GET /auth/streaming/accounts`로 확인하고, `DELETE /auth/streaming/accounts/{provider}`로 해제합니다.
2. **세션 생성과 방송 설정**: `POST /sessions`로 세션을 만들고, `PUT /sessions/{id}/broadcast`로 제목·설명·공개 범위·아동용 여부·카테고리·썸네일을 저장합니다. `GET /sessions/{id}/broadcast/defaults`는 폼을 채울 기본값을 줍니다.
3. **방송 준비**: `POST /sessions/{id}/stream/prepare`가 플랫폼에 방송을 만들고 RTMP 송출을 시작합니다. 이 단계에서는 시청자에게 보이지 않으니 화면을 먼저 확인할 수 있습니다.
4. **라이브 전환**: `POST /sessions/{id}/stream/golive`로 시청자에게 공개합니다.
5. **제어와 종료**: 진행 중에는 `stream/pause`와 `stream/resume`으로 송출을 멈췄다 재개합니다. `stream/stop`은 송출을 끊고 플랫폼 방송까지 정리하며, 뷰어 WebRTC와 세션은 그대로 둡니다.

`POST /sessions/{id}/stream/start`는 방송 생성과 송출을 한 번에 하고 autoStart로 라이브까지 넘기던 종전 경로입니다. **제거 예정**이며, 클라이언트가 준비·전환 두 경로로 옮겨가면 삭제합니다.

세션이 어떤 이유로 끝나든(WebRTC 실패, 복구 시간 초과, 로그아웃, 명시적 삭제) 서버가 플랫폼 방송을 단계에 맞춰 정리합니다. 준비까지만 간 방송은 삭제하고, 라이브였던 방송은 종료시킵니다. 플랫폼의 autoStop을 기다리면 다음 방송과 라이브가 겹치기 때문입니다.

## 주요 HTTP 엔드포인트

세션 경로는 사용자 인증(`requireUser`)과 세션 소유권 검사(`requireSessionOwner`)를 함께 거칩니다. 활성 세션 목록을 주는 엔드포인트는 두지 않았습니다. `session_id`를 전부 노출해 토큰 모델을 무력화하기 때문입니다.

### 공통

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `GET` | `/` | 서비스 이름과 기동 여부 |
| `GET` | `/health` | 서버 및 AI 런타임 상태 |
| `GET` | `/metrics` | Prometheus 메트릭 |
| `GET` | `/webrtc/config` | 클라이언트용 ICE 서버 설정과 복구 파라미터 |
| `GET` | `/signaling` | WebRTC 시그널링(WebSocket) |
| `GET` | `/client/` | 번들 웹 클라이언트 |
| `GET` | `/debug/pprof/` | 프로파일링. `PPROF_ENABLED=true`일 때만 등록합니다 |

### 세션 · 송출

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `POST` | `/sessions` | 세션 생성. 응답에 `owner_token`을 한 번만 실어 보냅니다 |
| `GET`/`DELETE` | `/sessions/{id}` | 세션 조회/삭제 |
| `PUT` | `/sessions/{id}/broadcast` | 방송 설정 저장(제목·설명·공개 범위·아동용 여부·카테고리·썸네일) |
| `GET` | `/sessions/{id}/broadcast/defaults` | 방송 설정 폼의 기본값 |
| `POST` | `/sessions/{id}/stream/prepare` | 방송 준비. 플랫폼 방송 생성 + RTMP 송출 시작(시청자에게 노출되지 않음) |
| `POST` | `/sessions/{id}/stream/golive` | 준비된 방송을 라이브로 전환 |
| `POST` | `/sessions/{id}/stream/pause`\|`/resume` | RTMP 송출 일시 중지/재개 |
| `POST` | `/sessions/{id}/stream/stop` | 송출 종료. 플랫폼 방송도 단계에 맞춰 정리(뷰어 WebRTC·세션은 유지) |
| `POST` | `/sessions/{id}/stream/start` | **제거 예정.** 종전 송출 시작. 방송 생성 + 송출, autoStart로 라이브까지 자동 전환 |
| `PATCH` | `/sessions/{id}/anonymization` | `{ "enabled": true\|false }`로 비식별화 AI 처리만 켜거나 끔(WebRTC·RTMP 송출 유지) |

### 참조 얼굴

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `GET`/`POST`/`DELETE` | `/reference-face` | 참조(본인) 얼굴 등록/조회/전체 삭제 |
| `DELETE` | `/reference-face/{face_id}` | 참조 얼굴 개별 삭제 |

### 인증

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `POST` | `/auth/sign-up` | 이메일·비밀번호로 인증 코드를 발송하고 `signup_token` HttpOnly 쿠키 설정 |
| `POST` | `/auth/verify-email` | 쿠키의 가입 토큰과 인증 코드를 검증해 User 생성 및 Redis 상태 정리 |
| `POST` | `/auth/native/sign-up` | 네이티브 앱용. JSON 본문의 `signup_token`을 반환 |
| `POST` | `/auth/native/verify-email` | 네이티브 앱용. JSON의 `signup_token`, `verification_code`를 검증 |
| `POST` | `/auth/sign-in` | 이메일·비밀번호 로그인 및 토큰 발급 |
| `POST` | `/auth/google` | Google ID 토큰을 검증하고 토큰 발급 |
| `POST` | `/auth/apple` | Apple authorization code를 교환·검증하고 토큰 발급 |
| `POST` | `/auth/refresh` | 리프레시 토큰으로 액세스 토큰 재발급 |
| `POST` | `/auth/logout` | 로그아웃. 해당 사용자의 활성 세션도 정리 |
| `DELETE` | `/auth/me` | 계정 탈퇴 |
| `POST` | `/auth/youtube/connect` | 인가 코드를 받아 YouTube 송출 계정 연결 |
| `GET` | `/auth/youtube/config` | 웹 클라이언트가 OAuth 팝업을 초기화할 공개 설정 |
| `GET` | `/auth/streaming/accounts` | 연결된 송출 계정 목록 |
| `DELETE` | `/auth/streaming/accounts/{provider}` | 송출 계정 연결 해제 |

## 관측성

`GET /metrics`가 `innolive_` 접두사의 Prometheus 메트릭을 노출합니다. 파이프라인을 진단할 때 먼저 보는 메트릭입니다.

- **세션**: `innolive_active_sessions`, `innolive_sessions_rejected_total`, `innolive_connection_failures_total`
- **AI 처리**: `innolive_ai_duration_seconds`, `innolive_ai_fallback_frames_total`, `innolive_ai_fallback_latched_total`, `innolive_ai_target_ready`
- **프레임 흐름**: `innolive_frame_received_total`, `innolive_frame_processed_total`, `innolive_frame_dropped_total`, `innolive_processing_queue_size`
- **송출**: `innolive_egress_frames_dropped_total`, `innolive_egress_reconnect_total`, `innolive_audio_samples_dropped_total`
- **WebRTC 복구**: `innolive_peer_recovery_started_total`, `innolive_peer_recovery_succeeded_total`, `innolive_peer_recovery_exhausted_total`

`innolive_ai_target_ready`는 상시 감시 프로브가 워커마다 갱신하는 게이지입니다. 프로세스가 살아 있어도 추론이 죽은 워커는 여기서 `0`이 됩니다.

## 배포

`main` 브랜치에 머지하면 GitHub Actions가 이미지를 빌드해 레지스트리에 올리고, 프로덕션 서버가 그 이미지를 받아 컨테이너를 재기동합니다. 배포 경로는 이 하나뿐입니다. 서버에 들어가 소스나 바이너리를 고치지 마세요. 자세한 절차와 비상 경로는 [`deploy/README.md`](deploy/README.md)에 있습니다.

## Third-party notices

Go 의존성 18개는 전부 허용형(MIT / BSD / Apache-2.0)입니다. 런타임 컨테이너에
포함되는 FFmpeg은 Debian이 `--enable-gpl`로 빌드하므로 GPL-2.0+ 조건입니다.
패키지별 라이선스는 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)를 참고하세요.

## 라이선스

이 프로젝트는 [Apache License 2.0](LICENSE) 하에 배포됩니다.
