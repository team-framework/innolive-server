# 제어 API

배포 스크립트(`apply-release.sh`)와 AI 워커 교체 배포가 같은 호스트에서 부르는 내부 API다.
앱·웹 클라이언트용 API가 아니며 Caddy를 거치지 않는다.

## 리스너와 인증

- `AI_CONTROL_ADDR`에 지정한 주소에만 연다. 루프백 IP만 받는다(예: blue `127.0.0.1:9091`, green `127.0.0.1:9092`). 비우면 제어 API를 열지 않는다.
- 모든 요청에 `Authorization: Bearer <AI_CONTROL_TOKEN>`이 필요하다. 토큰은 32자 이상이어야 하고, 두 슬롯이 같은 값을 쓴다(슬롯끼리 세션을 넘길 때도 이 토큰을 쓴다).
- 토큰이 없거나 틀리면 `401 {"error": "unauthorized"}`.
- 응답은 모두 `Content-Type: application/json`이다. 오류 본문은 `{"error": "<사유>"}`이며, 공개 API의 오류 형식(`code`·`message`)과 다르다.
- 토큰은 프로세스 인자로 넘기지 않는다. `apply-release.sh`처럼 curl의 `-H @-`로 stdin에서 넘긴다.

```bash
printf 'Authorization: Bearer %s\n' "$AI_CONTROL_TOKEN" |
  curl -sS -H @- -H 'Content-Type: application/json' \
    -d '{"to":"http://127.0.0.1:9092","phase":"prepare"}' \
    http://127.0.0.1:9091/sessions/handoff
```

## 세션 이관

서버 교체 배포(#517)에서 옛 슬롯이 세션을 새 슬롯으로 넘긴다. WebRTC·FFmpeg·AI 스트림은 넘기지 않고, 클라이언트가 같은 세션 id로 새 슬롯과 다시 협상하면 새로 만든다.

| 단계 | 하는 일 |
|---|---|
| `prepare` | 모든 세션을 새 슬롯에 재연결 대기 상태로 받아 둔다. 옛 슬롯은 그대로 서비스한다. 이 뒤에 Caddy를 새 슬롯으로 돌려도 세션 조회가 404가 되지 않는다. |
| `commit` | 최신 상태를 다시 보내고 옛 슬롯이 세션을 놓는다(PeerConnection·egress 종료). |

### `POST /sessions/handoff`

옛 슬롯에 보낸다. 옛 슬롯이 자기 세션을 하나씩 새 슬롯의 `POST /sessions/import`로 보낸다.

요청 본문(최대 4 KB)

| 필드 | 값 |
|---|---|
| `to` | 새 슬롯 제어 API 주소. `http://<루프백 IP>:<포트>`만 받는다(스냅샷에 스트림 키가 들어 있어서다). 경로는 비우거나 `/`. |
| `phase` | `prepare` 또는 `commit` |

응답 본문

| 필드 | 뜻 |
|---|---|
| `phase` | 실행한 단계 |
| `handed` | 새 슬롯이 받은 세션 수 |
| `detached` | 옛 슬롯이 놓은 세션 수(`commit`에서만 0이 아니다) |
| `skipped` | 넘기지 않은 세션 id와 사유. 게스트 세션(대기열 상태가 프로세스 밖에 있음)과 그사이 닫힌 세션이다. 옛 슬롯을 멈추면 함께 끝난다. |
| `failed` | 넘기지 못한 세션 id와 사유. 이 세션은 옛 슬롯에 그대로 남는다. |

| 상태 코드 | 뜻 |
|---|---|
| `200` | `failed`가 없다(`skipped`는 있을 수 있다). |
| `400` | 본문 형식, `to`가 루프백 `http` 주소가 아님, `phase` 값 오류 |
| `502` | 세션이 하나라도 `failed`에 들어갔다. |

동작 세부

- `commit`에서 플랫폼 왕복(방송 준비·라이브 전환)이나 해상도 변경 중인 세션은 0.5초 간격으로 최대 20번 다시 뜬다. `prepare`는 기다리지 않고 `failed`로 돌려준다.
- `commit`은 스냅샷을 보낸 뒤 그사이 세션이 바뀌었으면 다시 떠서 보낸다. 3번째에는 마지막 스냅샷으로 놓는다.
- import 요청 하나는 10초 제한이다. `commit`에서 응답을 받지 못한 전송 오류는 한 번 다시 보낸다(확정 import는 다시 보내도 결과가 같다).
- import 요청은 리다이렉트를 따라가지 않는다.
- 요청 전체 제한은 없다. `apply-release.sh`는 curl `--max-time 330`으로 부르고, 두 단계 모두 `200`이 아니면 실패로 본다. `prepare` 실패는 새 슬롯을 멈추고 끝나며(방송 영향 없음), `commit` 실패는 두 슬롯을 모두 둔 채 `MANUAL INTERVENTION REQUIRED`를 남긴다.

### `POST /sessions/import`

새 슬롯이 받는다. 옛 슬롯의 `POST /sessions/handoff`가 부르며 사람이 직접 부를 일은 없다.

- 쿼리: `commit=true`면 확정 import, 없으면 준비 import
- 본문: 세션 스냅샷(`session.SessionSnapshot` JSON, 최대 1 MB). 송출 URL(스트림 키)과 소유자 토큰 해시가 들어 있다.
- 성공: `200 {"session_id": "<id>", "committed": true|false}`

| 상태 코드 | 뜻 |
|---|---|
| `200` | 받았다. 이미 받은 준비 세션(클라이언트가 아직 붙지 않음)은 새 스냅샷으로 바꾼다. 클라이언트가 옛 슬롯이 놓기 전에 먼저 넘어온 세션에 확정 import가 오면 상태를 덮지 않고 확정만 반영한다. |
| `400` | 스냅샷 JSON 오류, 세션 id가 UUID가 아님, 소유자 토큰 해시 형식 오류, 송출 대상이 없음. 새 슬롯의 PeerConnection 생성 실패도 `400`으로 돌려준다. |
| `409` | 같은 id의 세션이 이미 이 슬롯에서 클라이언트와 연결돼 있다(이관이 끝난 세션을 덮어쓰지 않는다). |

## AI 워커 교체

AI 워커 교체 배포(#515)가 새 워커를 띄운 뒤 진행 중인 방송을 다음 프레임부터 넘기고 옛 워커를 뺀다.

### `GET /ai/workers`

풀의 워커 상태를 풀 순서대로 돌려준다.

```json
{"workers": [{"address": "127.0.0.1:50051", "ready": true, "draining": false, "face_swap": false, "active_streams": 2}]}
```

| 필드 | 뜻 |
|---|---|
| `ready` | 상태 점검을 통과했다. `false`면 새 세션을 배정하지 않는다. |
| `draining` | 교체 중이라 새 세션을 배정하지 않는다. 이미 붙은 세션은 그대로다. |
| `face_swap` | 얼굴 합성 워커 |
| `active_streams` | 이 워커에 붙은 스트림 수 |

AI가 꺼져 있으면 `503 {"error": "AI is disabled"}`.

### `POST /ai/workers/replace`

요청 본문(최대 4 KB)

| 필드 | 값 |
|---|---|
| `from` | 뺄 워커 주소(`host:port`) |
| `to` | 새 워커 주소(`host:port`) |
| `allow_reenrollment` | `true`면 화이트리스트 이동이 실패해도(옛 워커에 이동 RPC가 없는 릴리스이거나 인식 모델이 바뀜) 진행한다. 옮기지 못한 얼굴은 다음 대조에서 재등록 필요로 표시된다. 기본 `false`. |

진행 순서

1. `to`를 배정 제외로 풀에 더하고 프레임 왕복 preflight로 예열한다.
2. `from`의 화이트리스트를 엔트리 id 그대로 `to`에 옮긴다.
3. `to`의 배정을 열고 `from`을 배정 제외로 둔 뒤, `from`의 세션을 다음 프레임부터 넘긴다.
4. `from`에 남은 세션이 없으면 풀에서 뺀다.

2단계까지 실패하면 `to`를 풀에서 빼 처음 상태로 돌린다. 같은 요청을 다시 보내면 멈춘 단계부터 이어서 진행하고, 남은 세션만 다시 옮긴다. 요청 하나는 5분 제한이다.

응답 본문

| 필드 | 뜻 |
|---|---|
| `from`·`to` | 요청한 워커 |
| `copied_sessions` | 화이트리스트를 옮긴 AI 세션 수 |
| `pruned_entries` | 복사 중 지워진 얼굴이라 새 워커에서 다시 지운 엔트리 수 |
| `whitelist_skipped` | `allow_reenrollment`로 화이트리스트를 옮기지 않고 넘어간 사유 |
| `moved_sessions` | 새 워커로 넘긴 세션 수 |
| `failed_sessions` | 넘기지 못해 옛 워커에 남은 세션 id와 사유 |
| `removed` | 옛 워커를 풀에서 뺐는지 |

| 상태 코드 | 뜻 |
|---|---|
| `200` | 교체가 끝났고 옛 워커를 풀에서 뺐다. |
| `202` | 옛 워커에 세션이 남아 배정 제외로만 두었다. 아직 옛 워커를 끄면 안 된다. 같은 요청을 다시 보낸다. |
| `400` | 본문 형식, `from`·`to` 누락, 두 주소가 같음 |
| `404` | `from`이 풀에 없다. |
| `409` | 두 워커의 인식(임베딩) 모델이 달라 화이트리스트를 옮길 수 없다. `allow_reenrollment`로 진행할지 정한다. |
| `502` | 그 밖의 실패(예열·화이트리스트 이동·세션 이동) |
| `503` | AI가 꺼져 있다. |

오류 응답은 `{"error": "<사유>", "result": {...}}`이며 `result`는 실패 시점까지의 위 응답 본문이다.
