#!/usr/bin/env bash
# 실제 배포를 수행하는 스크립트(root, sudoers 한 줄로만 호출 허용).
# 사용법: apply-release.sh <sha-tag> [--force]
#
# 무중단 배포(#517): 쉬고 있는 슬롯(blue·green)에 새 태그를 띄우고, 지금 슬롯의 방송
# 세션을 새 슬롯으로 넘긴 뒤 지금 슬롯을 멈춘다. 클라이언트는 같은 세션 id로 새 슬롯과
# 다시 협상하므로 방송은 수 초 끊긴 뒤 이어진다.
#   1. 새 슬롯 기동 + /health 확인          (실패: 새 슬롯을 멈추고 끝. 방송 영향 없음)
#   2. 세션 이관 준비(prepare)              (실패: 새 슬롯을 멈추고 끝. 방송 영향 없음)
#   3. Caddy 업스트림을 새 슬롯으로 전환     (실패: 되돌리고 새 슬롯을 멈추고 끝)
#   4. 세션 이관 확정(commit)               (옛 슬롯이 세션을 놓고 클라이언트가 새 슬롯에 붙는다)
#   5. 옛 슬롯에 남은 세션(이관하지 않는 게스트)이 끝나길 기다린 뒤 옛 슬롯 정지
#      --force: 기다리지 않고 바로 멈춘다(남은 게스트 세션이 끊긴다).
#
# 출력 계약: stdout은 GitHub Actions 러너 로그로 전달되어 공개된다.
#   허용 출력: "GATE: ...", "PULL OK", "HANDOFF OK <n>", "DEPLOY OK <sha>", "DEPLOY DEFERRED",
#             "ROLLBACK OK", "FAILED: see server journal", "MANUAL INTERVENTION REQUIRED"
#   금지: set -x, env/secrets 파일 내용, journalctl/docker logs/docker inspect/
#         docker compose config 출력, 상세 에러 메시지.
#   상세 진단은 /opt/innolive/deploy/logs/ 아래(root 600)에만 기록한다.
set -euo pipefail

TAG="${1:?tag required}"
FORCE="${2:-}"

# 서버 로컬 설정(레포에 없는 값): INNOLIVE_IMAGE(레지스트리 이미지 경로),
# DOCKERHUB_USER, INNOLIVE_APPLE_KEY_PATH, CADDYFILE(호스트의 Caddyfile 경로),
# CADDY_CONTAINER(Caddy 컨테이너 이름) 등을 정의한다.
# shellcheck disable=SC1091
source /etc/innolive/deploy.env

TAG_DIR="/opt/innolive/deploy"
TOKEN_FILE="/etc/innolive/dockerhub.token"
SERVER_ENV="/etc/innolive/server.env"
LOG_DIR="/opt/innolive/deploy/logs"
DETAIL_LOG="${LOG_DIR}/deploy-$(date +%Y%m%d-%H%M%S)-${TAG:0:7}.log"
CADDY_CONTAINER="${CADDY_CONTAINER:-innolive-caddy}"
# 옛 슬롯에 남은(이관하지 않는) 세션을 기다리는 시간. 30초 x 20 = 10분.
DRAIN_CHECKS=20

mkdir -p "${LOG_DIR}"
touch "${DETAIL_LOG}"
chmod 600 "${DETAIL_LOG}"

detail() { printf '%s %s\n' "$(date '+%F %T')" "$*" >>"${DETAIL_LOG}"; }

fail() {
  detail "FATAL: $*"
  echo "FAILED: see server journal"
  exit 1
}

slot_value() {
  # slot_value <slot> <KEY> — 슬롯 env 파일의 값
  sed -n "s/^$2=//p" "/etc/innolive/server-slot-$1.env" | tail -1
}

http_port() { slot_value "$1" HTTP_ADDR | sed 's/.*://'; }
control_addr() { slot_value "$1" AI_CONTROL_ADDR; }

health_ok() {
  curl -fsS -o /dev/null --max-time 3 "http://localhost:$(http_port "$1")/health" 2>>"${DETAIL_LOG}"
}

active_sessions() {
  # 서버가 내려가 있으면(metrics 응답 없음) 0으로 취급한다.
  local out
  out="$(curl -fsS --max-time 3 "http://localhost:$(http_port "$1")/metrics" 2>>"${DETAIL_LOG}")" || { echo 0; return 0; }
  printf '%s\n' "${out}" \
    | awk '/^innolive_active_sessions /{print int($2); found=1} END{if(!found) print 0}'
}

control_post() {
  # control_post <slot> <path> <json> — 응답 본문은 상세 로그에만 남기고 HTTP 코드를 출력한다
  local token
  token="$(sed -n 's/^AI_CONTROL_TOKEN=//p' "${SERVER_ENV}" | tail -1)"
  curl -sS -o >(cat >>"${DETAIL_LOG}") -w '%{http_code}' --max-time 330 \
    -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
    --data "$3" "http://$(control_addr "$1")$2" 2>>"${DETAIL_LOG}" || echo 000
}

# switch_caddy <port> — Caddy가 미디어 서버로 보내는 업스트림 포트를 바꾼다. Caddyfile은
# 컨테이너에 파일 하나로 바인드 마운트돼 있어, 새 파일로 바꾸면(inode 교체) 컨테이너가
# 옛 내용을 계속 본다. 그래서 같은 파일에 덮어쓴다.
switch_caddy() {
  local port="$1" next
  [[ -n "${CADDYFILE:-}" && -f "${CADDYFILE}" ]] || { detail "CADDYFILE not set"; return 1; }
  next="$(mktemp)"
  sed -E "s#reverse_proxy host\.docker\.internal:[0-9]+#reverse_proxy host.docker.internal:${port}#" "${CADDYFILE}" >"${next}"
  grep -q "reverse_proxy host.docker.internal:${port}" "${next}" || { rm -f "${next}"; detail "upstream line not found"; return 1; }
  cat "${next}" >"${CADDYFILE}"
  rm -f "${next}"
  docker exec "${CADDY_CONTAINER}" caddy reload --config /etc/caddy/Caddyfile >>"${DETAIL_LOG}" 2>&1
}

detail "deploy start tag=${TAG} force=${FORCE:-no}"

# ── 슬롯 결정 ────────────────────────────────────────────────────
if systemctl is-active --quiet innolive-server@blue; then
  ACTIVE=blue NEXT=green
elif systemctl is-active --quiet innolive-server@green; then
  ACTIVE=green NEXT=blue
else
  # 도는 슬롯이 없으면(첫 설치·장애 복구) 이관할 세션도 없다. blue로 바로 띄운다.
  ACTIVE="" NEXT=blue
fi
for slot in blue green; do
  [[ -f "/etc/innolive/server-slot-${slot}.env" ]] || fail "missing slot env ${slot}"
done
detail "active=${ACTIVE:-none} next=${NEXT}"
if systemctl is-active --quiet "innolive-server@${NEXT}"; then
  fail "next slot ${NEXT} is already running"
fi

# ── 이미지 pull ──────────────────────────────────────────────────
docker login docker.io -u "${DOCKERHUB_USER}" --password-stdin \
  <"${TOKEN_FILE}" >>"${DETAIL_LOG}" 2>&1 || fail "docker login failed"
docker pull "${INNOLIVE_IMAGE}:${TAG}" >>"${DETAIL_LOG}" 2>&1 || fail "docker pull failed"
echo "PULL OK"

# ── 재시작 전 런타임 로그 보관 ──────────────────────────────────
/opt/innolive/deploy/log-archive.sh >>"${DETAIL_LOG}" 2>&1 || detail "log archive failed"

# ── 1. 새 슬롯 기동 + 헬스 확인 ──────────────────────────────────
printf 'INNOLIVE_TAG=%s\n' "${TAG}" >"${TAG_DIR}/current_tag-${NEXT}"
chmod 644 "${TAG_DIR}/current_tag-${NEXT}"
systemctl start "innolive-server@${NEXT}" >>"${DETAIL_LOG}" 2>&1 || detail "start ${NEXT} exited nonzero"

started=false
for _ in $(seq 1 40); do
  sleep 3
  if health_ok "${NEXT}"; then
    started=true
    break
  fi
done
if [[ "${started}" != "true" ]]; then
  systemctl stop "innolive-server@${NEXT}" >>"${DETAIL_LOG}" 2>&1 || true
  detail "new slot failed health check; previous slot keeps serving"
  [[ -n "${ACTIVE}" ]] && echo "ROLLBACK OK"
  fail "health check failed for ${NEXT}"
fi

if [[ -z "${ACTIVE}" ]]; then
  switch_caddy "$(http_port "${NEXT}")" || fail "caddy switch failed"
  systemctl enable --quiet "innolive-server@${NEXT}"
  echo "DEPLOY OK ${TAG}"
  exit 0
fi

handoff_body() { printf '{"to":"http://%s","phase":"%s"}' "$(control_addr "${NEXT}")" "$1"; }

# ── 2. 세션 이관 준비 ────────────────────────────────────────────
code="$(control_post "${ACTIVE}" /sessions/handoff "$(handoff_body prepare)")"
detail "handoff prepare http=${code}"
if [[ "${code}" != "200" ]]; then
  systemctl stop "innolive-server@${NEXT}" >>"${DETAIL_LOG}" 2>&1 || true
  echo "ROLLBACK OK"
  fail "handoff prepare failed"
fi

# ── 3. Caddy 전환 ────────────────────────────────────────────────
if ! switch_caddy "$(http_port "${NEXT}")"; then
  switch_caddy "$(http_port "${ACTIVE}")" || detail "caddy revert failed"
  systemctl stop "innolive-server@${NEXT}" >>"${DETAIL_LOG}" 2>&1 || true
  echo "ROLLBACK OK"
  fail "caddy switch failed"
fi

# ── 4. 세션 이관 확정 ────────────────────────────────────────────
code="$(control_post "${ACTIVE}" /sessions/handoff "$(handoff_body commit)")"
detail "handoff commit http=${code}"
if [[ "${code}" != "200" ]]; then
  # 넘기지 못한 세션은 옛 슬롯에 남아 있다. 두 슬롯을 모두 둔 채 사람이 본다.
  echo "MANUAL INTERVENTION REQUIRED"
  fail "handoff commit failed for some sessions"
fi
echo "HANDOFF OK $(active_sessions "${NEXT}")"

systemctl enable --quiet "innolive-server@${NEXT}"
systemctl disable --quiet "innolive-server@${ACTIVE}"

# ── 5. 옛 슬롯 정리 ──────────────────────────────────────────────
if [[ "${FORCE}" != "--force" ]]; then
  for _ in $(seq 1 "${DRAIN_CHECKS}"); do
    remaining="$(active_sessions "${ACTIVE}")"
    [[ "${remaining}" == "0" ]] && break
    echo "GATE: waiting (${remaining} sessions)"
    detail "drain wait sessions=${remaining}"
    sleep 30
  done
fi
systemctl stop "innolive-server@${ACTIVE}" >>"${DETAIL_LOG}" 2>&1 || detail "stop ${ACTIVE} exited nonzero"
detail "deploy ok tag=${TAG} slot=${NEXT}"
echo "DEPLOY OK ${TAG}"
