# Third-Party Notices

이 저장소에 포함되거나 런타임에 사용되는 제3자 소프트웨어의 라이선스 고지입니다.

Go 서버 바이너리에 링크되는 모듈 목록은 `go list -deps ./cmd/server` 기준입니다.
라이선스 본문과 NOTICE는 [`third_party/licenses/`](third_party/licenses/)에 있습니다.
배포 이미지에서는 `/usr/share/doc/innolive-server/`로 복사됩니다.

목록을 다시 만들려면 `make licenses`를 실행하세요.

## Go 런타임 의존성

`./cmd/server`에 링크되는 제3자 모듈 59개입니다. 전부 허용형(MIT / BSD / Apache-2.0)입니다.

| 모듈 | 라이선스 |
| --- | --- |
| `cloud.google.com/go/auth` | Apache-2.0 |
| `cloud.google.com/go/auth/oauth2adapt` | Apache-2.0 |
| `cloud.google.com/go/compute/metadata` | Apache-2.0 |
| `github.com/cespare/xxhash/v2` | MIT |
| `github.com/dgryski/go-rendezvous` | MIT |
| `github.com/felixge/httpsnoop` | MIT |
| `github.com/go-logr/logr` | Apache-2.0 |
| `github.com/go-logr/stdr` | Apache-2.0 |
| `github.com/golang-jwt/jwt/v5` | MIT |
| `github.com/golang-migrate/migrate/v4` | MIT |
| `github.com/google/s2a-go` | Apache-2.0 |
| `github.com/google/uuid` | BSD-3-Clause |
| `github.com/googleapis/enterprise-certificate-proxy` | Apache-2.0 |
| `github.com/googleapis/gax-go/v2` | BSD-3-Clause |
| `github.com/gorilla/websocket` | BSD-2-Clause |
| `github.com/jackc/pgpassfile` | MIT |
| `github.com/jackc/pgservicefile` | MIT |
| `github.com/jackc/pgx/v5` | MIT |
| `github.com/jackc/puddle/v2` | MIT |
| `github.com/jinzhu/inflection` | MIT |
| `github.com/jinzhu/now` | MIT |
| `github.com/lib/pq` | MIT |
| `github.com/pion/datachannel` | MIT |
| `github.com/pion/dtls/v3` | MIT |
| `github.com/pion/ice/v4` | MIT |
| `github.com/pion/interceptor` | MIT |
| `github.com/pion/logging` | MIT |
| `github.com/pion/mdns/v2` | MIT |
| `github.com/pion/randutil` | MIT |
| `github.com/pion/rtcp` | MIT |
| `github.com/pion/rtp` | MIT |
| `github.com/pion/sctp` | MIT |
| `github.com/pion/sdp/v3` | MIT |
| `github.com/pion/srtp/v3` | MIT |
| `github.com/pion/stun/v3` | MIT |
| `github.com/pion/transport/v3` | MIT |
| `github.com/pion/turn/v4` | MIT |
| `github.com/pion/webrtc/v4` | MIT |
| `github.com/redis/go-redis/v9` | BSD-2-Clause |
| `github.com/wlynxg/anet` | BSD-3-Clause |
| `go.opentelemetry.io/auto/sdk` | Apache-2.0 |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | Apache-2.0 |
| `go.opentelemetry.io/otel` | Apache-2.0 |
| `go.opentelemetry.io/otel/metric` | Apache-2.0 |
| `go.opentelemetry.io/otel/trace` | Apache-2.0 |
| `go.uber.org/atomic` | MIT |
| `golang.org/x/crypto` | BSD-3-Clause |
| `golang.org/x/image` | BSD-3-Clause |
| `golang.org/x/net` | BSD-3-Clause |
| `golang.org/x/oauth2` | BSD-3-Clause |
| `golang.org/x/sync` | BSD-3-Clause |
| `golang.org/x/sys` | BSD-3-Clause |
| `golang.org/x/text` | BSD-3-Clause |
| `google.golang.org/api` | BSD-3-Clause |
| `google.golang.org/genproto/googleapis/rpc` | Apache-2.0 |
| `google.golang.org/grpc` | Apache-2.0 |
| `google.golang.org/protobuf` | BSD-3-Clause |
| `gorm.io/driver/postgres` | MIT |
| `gorm.io/gorm` | MIT |

`github.com/alicebob/miniredis/v2`는 테스트 전용이며 서버 바이너리에 포함되지 않습니다.

## FFmpeg

Dockerfile이 Debian bookworm의 `ffmpeg` 패키지를 설치합니다.
Debian은 `--enable-gpl`과 `libx264`를 포함해 빌드하므로 이 바이너리의 라이선스는
**GPL-2.0-or-later**입니다. 패키지 저작권 고지는 이미지의
`/usr/share/doc/ffmpeg/`에 있습니다. 서버는 FFmpeg을 서브프로세스로 실행하며
링크하지 않습니다.

## 컨테이너 이미지

| 이미지 | 용도 |
| --- | --- |
| `golang:1.25-bookworm` | 빌드 단계. 최종 이미지에 포함되지 않습니다. |
| `debian:bookworm-slim` | 런타임 |

프로젝트의 Apache License 2.0은 InnoLive 자체 소스 코드에만 적용됩니다.
제3자 소프트웨어는 각자의 라이선스를 따릅니다.
