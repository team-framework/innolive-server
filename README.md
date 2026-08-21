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
| `YOUTUBE_STREAM_KEY` | — | YouTube RTMP 스트림 키 |
| `ENABLE_AUDIO_EGRESS` | `false` | 발행자 마이크 오디오 송출 여부 |
| `DATABASE_URL` | — | PostgreSQL 연결 문자열 |
| `DATABASE_MIGRATION_MODE` | `auto` | `auto` / `versioned` / `off` |
| `AUTH_EMAIL_SMTP_HOST` | — | SMTP 호스트. 비어 있으면 이메일 로그인 API가 비활성화됨 |
| `AUTH_EMAIL_REDIS_ADDR` | — | 가입 대기 정보·인증 코드를 보관하는 Redis 주소 |
| `AUTH_EMAIL_VERIFICATION_CODE_TTL` | `5m` | 회원가입 인증 코드 만료 시간 |

## 주요 HTTP 엔드포인트

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `GET` | `/health` | 서버 및 AI 런타임 상태 |
| `GET` | `/metrics` | Prometheus 메트릭 |
| `GET` | `/webrtc/config` | 클라이언트용 ICE 서버 설정 |
| `POST` | `/sessions` | 세션 생성 (소유권 토큰 발급) |
| `GET`/`DELETE` | `/sessions/{id}` | 세션 조회/삭제 |
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
