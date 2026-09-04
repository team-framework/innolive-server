# Third-Party Notices

이 저장소에 포함되거나 런타임에 사용되는 제3자 소프트웨어의 라이선스 고지입니다.
`go.sum`이 Go 의존성 전체 트리의 정본 인벤토리입니다.

## Go 직접 의존성

| 패키지 | 라이선스 |
| --- | --- |
| `pion/webrtc` · `rtp` · `sdp` · `logging` | MIT |
| `gorilla/websocket` | BSD-2-Clause |
| `golang-jwt/jwt` | MIT |
| `golang-migrate/migrate` | MIT |
| `google/uuid` | BSD-3-Clause |
| `redis/go-redis` | BSD-2-Clause |
| `golang.org/x/crypto` · `image` · `sys` | BSD-3-Clause |
| `google.golang.org/api` | BSD-3-Clause |
| `google.golang.org/grpc` | Apache-2.0 |
| `google.golang.org/protobuf` | BSD-3-Clause |
| `gorm.io/gorm` · `driver/postgres` | MIT |
| `alicebob/miniredis` | MIT |

## FFmpeg

Dockerfile이 Debian bookworm의 `ffmpeg` 패키지를 설치합니다.
Debian은 `--enable-gpl`과 `libx264`를 포함해 빌드하므로 이 바이너리의 라이선스는
**GPL-2.0-or-later**입니다.

## 컨테이너 베이스 이미지

| 이미지 | 용도 |
| --- | --- |
| `golang:1.25-bookworm` | 빌드 단계 |
| `debian:bookworm-slim` | 런타임 |

프로젝트의 Apache License 2.0은 InnoLive 자체 소스 코드에만 적용됩니다.
제3자 소프트웨어는 각자의 라이선스를 따릅니다.
