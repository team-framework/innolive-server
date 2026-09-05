# inno-live-server

실시간 WebRTC 라이브 방송을 받아 **AI 프라이버시 처리(얼굴 블러/치환)** 를 거친 뒤, 처리된 영상을 뷰어에게 전달하고 YouTube(RTMP)로 송출하는 Go 미디어 서버입니다.

발행자(publisher)의 카메라 프레임이 서버로 들어오면, 각 프레임은 AI 서버로 전달되어 등록된 "본인 얼굴"을 제외한 나머지 얼굴을 블러 처리한 뒤 뷰어와 YouTube 라이브로 나갑니다. 즉, **방송 화면에 실수로 노출되는 주변 사람들의 얼굴을 자동으로 가려주는 라이브 방송 서버**입니다.

---

## 이 프로젝트가 해결하는 문제

야외·실내 라이브 방송에서는 발행자 의도와 무관하게 지나가는 사람들의 얼굴이 그대로 송출됩니다. inno-live-server는 방송 파이프라인 중간에 AI 프라이버시 단계를 넣어, **본인 얼굴만 남기고 나머지 얼굴을 실시간으로 블러 처리**합니다. 처리 과정에서 문제가 생기면 원본이 새어나가지 않도록 화면을 차단(블랙아웃)하는 방향으로 안전하게 동작합니다.

## 주요 기능

- **WebRTC 인제스트/전달** — [pion/webrtc](https://github.com/pion/webrtc) 기반. STUN/TURN, ICE 재연결 유예 지원.
- **AI 프라이버시 파이프라인** — 프레임을 AI 서버로 보내 얼굴 블러/치환. `bypass`, `fixed_delay`, `real` 세 가지 모드.
- **안전한 실패 처리** — AI 처리가 지연·실패하면 미처리 원본이 나가지 않도록 화면을 차단.
- **YouTube RTMP 송출** — 처리된 영상 + 발행자 마이크(Opus) 오디오를 FFmpeg로 라이브 송출.
- **참조 얼굴 관리** — 블러에서 제외할 "본인 얼굴"을 REST API로 등록/삭제.
- **세션 소유권 인증** — 세션마다 발급되는 토큰으로 소유자만 제어 가능.
- **인증 기반 사용자 관리** — PostgreSQL + GORM. Google/Apple OAuth 및 이메일 인증 기반 계정과 로그인 세션 관리.
- **관측성** — Prometheus 메트릭, 구조화 JSON 로그, pprof 프로파일링.

## 기술 스택

| 영역 | 사용 기술 |
|------|-----------|
| 언어/런타임 | Go 1.25 |
| WebRTC | pion/webrtc v4, pion/rtp |
| AI 연동 | gRPC, protobuf |
| 미디어 트랜스코딩 | FFmpeg |
| DB | PostgreSQL 16, GORM, golang-migrate |
| 배포 | Docker / Docker Compose |

## 아키텍처

```
[Publisher] --WebRTC--> ┌──────────────── inno-live-server ────────────────┐
                        │                                                  │
                        │  signaling ─► session.Manager ─► media.Processo  │
                        │                                        │         │
                        │                                        ▼         │
                        │                              gRPC ─► [AI Server] │  얼굴 블러/치환
                        │                                        │         │
                        │                          (처리된 프레임)    ㅤ       │
                        │                          ├─► WebRTC ─────────────┼──► [Viewer]
                        │                          └─► RTMP 송출 (FFmpeg) ──┼──► [YouTube]
                        │                                                  │
                        │  server (HTTP/REST) ◄──► PostgreSQL (auth)       │
                        └──────────────────────────────────────────────────┘
```

### 코드 구조

```
cmd/server/            진입점: 설정 로드, DB/마이그레이션, 서버 부트스트랩
internal/
├── config/            환경변수 기반 설정 로드/검증
├── server/            HTTP 라우팅, REST 핸들러, 시그널링, 세션 소유권 인증
├── session/           세션 매니저, 소유권, 시그널링 상태
├── media/             WebRTC 트랙, AI 프로세서, FFmpeg, RTMP 송출, 오디오 파이프
├── ai/                AI gRPC 클라이언트 풀, preflight
├── auth/              User / OAuthAccount / RefreshSession 모델, 마이그레이션
├── database/          PostgreSQL 연결·커넥션 풀, 마이그레이션
└── metrics/           Prometheus 레지스트리, 프로세스 메트릭
api/proto/             AI 프로세서 gRPC 정의 (+ 생성된 코드)
```

## 시작하기

### 사전 요구사항

- Go 1.25+
- FFmpeg (모든 프라이버시 모드에서 필수)
- PostgreSQL 16 (Docker Compose로 함께 실행 가능)
- (선택) AI 서버 — `real` 모드에서 gRPC로 연결

### Docker Compose로 실행

```bash
cp .env.example .env
# .env에서 POSTGRES_PASSWORD 설정
docker compose up --build
```

서버는 `:8000`(HTTP), `50002-50020/udp`(WebRTC)로 노출됩니다.

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

## 설정

모든 설정은 환경변수로 주입합니다. 전체 목록과 기본값은 [`.env.example`](.env.example)를 참고하세요. 주요 항목:

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `HTTP_ADDR` | `:8000` | HTTP 리스닝 주소 |
| `AI_PRIVACY_MODE` | `real` | `bypass` / `fixed_delay` / `real` |
| `AI_GRPC_TARGETS` | — | AI 서버 gRPC 타겟(들) |
| `AI_FAILURE_POLICY` | `blackout_latch` | AI 실패 시 정책 (`blackout_latch` / `freeze`) |
| `FFMPEG_PATH` | `ffmpeg` | FFmpeg 실행 경로 |
| `WEBRTC_STUN_URLS` / `WEBRTC_TURN_*` | — | ICE 서버 설정 |
| `WEBRTC_UDP_PORT_MIN/MAX` | `50002` / `50020` | WebRTC UDP 포트 범위 |
| `WEBRTC_RECOVERY_WINDOW` | `50s` | 네트워크 전환 뒤 ICE restart를 기다리는 세션 유지 시간 |
| `WEBRTC_RECOVERY_DEBOUNCE` / `WEBRTC_RECOVERY_MAX_ATTEMPTS` | `2s` / `10` | 일시 끊김 필터와 서버가 수락할 ICE restart offer 상한 |
| `YOUTUBE_STREAM_KEY` | — | YouTube RTMP 스트림 키 |
| `ENABLE_AUDIO_EGRESS` | `false` | 발행자 마이크 오디오 송출 여부 |
| `DATABASE_URL` | — | PostgreSQL 연결 문자열 |
| `DATABASE_MIGRATION_MODE` | `auto` | `auto` / `versioned` / `off` |
| `AUTH_EMAIL_SMTP_HOST` | — | SMTP 호스트. 비어 있으면 이메일 로그인 API가 비활성화됨 |
| `AUTH_EMAIL_REDIS_ADDR` | — | 가입 대기 정보·인증 코드를 보관하는 Redis 주소 |
| `AUTH_EMAIL_VERIFICATION_CODE_TTL` | `5m` | 회원가입 인증 코드 만료 시간 |

### 비인증 체험 대기열 운영 계약

`GUEST_QUEUE_ENABLED=true`이면 비인증 사용자는 `POST /guest-queue`를 통해서만
체험 WebRTC 세션을 만들 수 있습니다. 이 기능은 개발용 인증 우회가 아니므로
`INNOLIVE_REQUIRE_SESSION_AUTH=true`를 유지합니다.

- `MAX_SESSIONS`는 반드시 `2` 이상이어야 합니다. 게스트의 활성 세션과 60초 입장
  예약을 합친 수는 `floor(MAX_SESSIONS × 0.5)`를 넘을 수 없고, 로그인 사용자는
  이 게스트 상한의 영향을 받지 않습니다.
- `GUEST_QUEUE_REDIS_ADDR`은 활성화 시 필수입니다. 인증 이메일용 Redis와 같은
  인스턴스를 사용해도 되며 게스트 키는 별도 접두어로 분리됩니다. Redis를 사용할 수
  없으면 게스트 큐와 게스트 세션 생성은 `503 queue_unavailable`으로 실패합니다.
- 대기표는 `GUEST_QUEUE_TTL`(기본 10분) 안에 heartbeat가 없으면 만료됩니다. SSE는
  순번 알림용이며 TTL을 연장하지 않습니다. 대기열은 최대 100명이고, 게스트 생성은
  IP별 5회/분 및 30회/시간으로 제한됩니다. 초과 응답은 `429`와 `Retry-After`입니다.
- admission 토큰은 한 번만 쓸 수 있으며 `GUEST_ADMISSION_TTL`(기본 60초) 안에
  소비하지 않으면 예약이 회수됩니다. 게스트 세션은 `GUEST_SESSION_TTL`(기본 10분)
  뒤 종료되고, 명시적 삭제·협상 실패·서버 종료를 포함한 모든 종료 경로에서 등록한
  기준 얼굴과 AI whitelist를 제거합니다.
- 허용된 특정 CORS origin에는 credential 응답이 적용됩니다. 게스트 대기열을 켠
  운영 환경에서는 `AUTH_CORS_ALLOW_ALL_ORIGINS=true`를 사용할 수 없고,
  `AUTH_CORS_ALLOWED_ORIGINS`에 기존 운영 웹 origin(예: `https://innolive.studio`)이
  포함되어 있어야 합니다.
- reverse proxy 또는 ingress를 거치면 `GUEST_QUEUE_TRUSTED_PROXY_CIDRS`에 서버로
  직접 접속하는 proxy CIDR만 설정합니다. 비워 두면 TCP peer IP를 사용합니다.

Prometheus에서는 `innolive_guest_queue_waiting`, `innolive_guest_active_sessions`,
`innolive_guest_admission_reservations`, `innolive_guest_queue_admitted_total`,
`innolive_guest_queue_expired_total`, `innolive_guest_rate_limited_total`로 대기열
상태를 관측합니다.

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

## 주요 HTTP 엔드포인트

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `GET` | `/health` | 서버 및 AI 런타임 상태 |
| `GET` | `/metrics` | Prometheus 메트릭 |
| `GET` | `/webrtc/config` | 클라이언트용 ICE 서버 설정 |
| `POST` | `/sessions` | 세션 생성 (소유권 토큰 발급) |
| `GET`/`DELETE` | `/sessions/{id}` | 세션 조회/삭제 |
| `POST` | `/guest-queue` | 게스트 대기표 생성 또는 기존 대기·활성 상태 조회 |
| `GET`/`DELETE` | `/guest-queue/{ticket_id}` | 게스트 대기 상태 조회/취소 |
| `POST` | `/guest-queue/{ticket_id}/heartbeat` | 게스트 대기표 TTL 연장 |
| `GET` | `/guest-queue/{ticket_id}/events` | 게스트 순번·입장 SSE 이벤트 |
| `POST` | `/guest-sessions` | admission 토큰 소비 및 게스트 세션 생성 (owner token·ICE 설정 반환) |
| `GET`/`DELETE` | `/guest/sessions/{id}` | 게스트 세션 조회/삭제 (쿠키와 owner token 필요) |
| `PATCH` | `/guest/sessions/{id}/anonymization` | 게스트 비식별화 설정 변경 (쿠키와 owner token 필요) |
| `GET`/`POST`/`DELETE` | `/guest/sessions/{id}/reference-face` | 게스트 세션별 기준 얼굴 관리 (쿠키와 owner token 필요) |
| `POST` | `/sessions/{id}/stream/start` | **제거 예정.** 종전 송출 시작 — 방송 생성 + 송출, autoStart로 라이브까지 자동 전환. 클라이언트가 아래 두 경로로 옮겨가면 삭제한다 |
| `POST` | `/sessions/{id}/stream/prepare` | 방송 준비 — 플랫폼 방송 생성 + RTMP 송출 시작 (시청자에게 노출되지 않음) |
| `POST` | `/sessions/{id}/stream/golive` | 준비된 방송을 라이브로 전환 |
| `POST` | `/sessions/{id}/stream/pause`\|`/resume`\|`/stop` | RTMP 송출 일시 중지/재개/중지 |
| `PATCH` | `/sessions/{id}/anonymization` | `{ "enabled": true\|false }`로 비식별화 AI 처리만 켜거나 끔 (WebRTC·RTMP 송출 유지) |
| `GET`/`POST`/`DELETE` | `/reference-face` | 참조(본인) 얼굴 등록/조회/삭제 |
| `GET` | `/signaling` | WebRTC 시그널링(WebSocket) |
| `POST` | `/auth/sign-up` | 이메일·비밀번호로 인증 코드를 발송하고 `signup_token` HttpOnly 쿠키 설정 |
| `POST` | `/auth/verify-email` | 쿠키의 가입 토큰과 인증 코드를 검증해 User 생성 및 Redis 상태 정리 |
| `POST` | `/auth/native/sign-up` | 네이티브 앱용. JSON 본문의 `signup_token`을 반환 |
| `POST` | `/auth/native/verify-email` | 네이티브 앱용. JSON의 `signup_token`, `verification_code`를 검증 |
| `POST` | `/auth/sign-in` | 이메일·비밀번호 로그인 및 토큰 발급 |
| `POST` | `/auth/apple` | Apple authorization code를 교환·검증하고 토큰 발급. 서버의 `APPLE_CLIENT_ID` 설정을 사용 |

## 라이선스

이 프로젝트는 [Apache License 2.0](LICENSE) 하에 배포됩니다.
