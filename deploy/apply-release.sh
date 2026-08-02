#!/usr/bin/env bash
# 실제 배포를 수행하는 스크립트(root, sudoers 한 줄로만 호출 허용).
# 사용법: apply-release.sh <sha-tag> [--force]
#
# 출력 계약: stdout은 GitHub Actions 러너 로그로 전달되어 공개된다.
#   허용 출력: "GATE: ...", "PULL OK", "DEPLOY OK <sha>", "DEPLOY DEFERRED",
#             "ROLLBACK OK", "FAILED: see server journal", "MANUAL INTERVENTION REQUIRED"
#   금지: set -x, env/secrets 파일 내용, journalctl/docker logs/docker inspect/
#         docker compose config 출력, 상세 에러 메시지.
#   상세 진단은 /opt/innolive/deploy/logs/ 아래(root 600)에만 기록한다.
set -euo pipefail

TAG="${1:?tag required}"
FORCE="${2:-}"

# 서버 로컬 설정(레포에 없는 값): INNOLIVE_IMAGE(레지스트리 이미지 경로),
# DOCKERHUB_USER, INNOLIVE_APPLE_KEY_PATH 등을 정의한다.
# shellcheck disable=SC1091
source /etc/innolive/deploy.env

COMPOSE_FILE="/opt/innolive/compose.prod.yaml"
TAG_FILE="/opt/innolive/deploy/current_tag"
TOKEN_FILE="/etc/innolive/dockerhub.token"
LOG_DIR="/opt/innolive/deploy/logs"
DETAIL_LOG="${LOG_DIR}/deploy-$(date +%Y%m%d-%H%M%S)-${TAG:0:7}.log"

mkdir -p "${LOG_DIR}"
touch "${DETAIL_LOG}"
chmod 600 "${DETAIL_LOG}"

detail() { printf '%s %s\n' "$(date '+%F %T')" "$*" >>"${DETAIL_LOG}"; }

fail() {
  detail "FATAL: $*"
  echo "FAILED: see server journal"
  exit 1
}

health_ok() {
  curl -fsS -o /dev/null --max-time 3 http://localhost:8000/health 2>>"${DETAIL_LOG}"
}

active_sessions() {
  # 서버가 내려가 있으면(metrics 응답 없음) 0으로 취급한다 — 복구 배포를 막지 않기 위함.
  curl -fsS --max-time 3 http://localhost:8000/metrics 2>>"${DETAIL_LOG}" \
    | awk '/^innolive_active_sessions /{print int($2); found=1} END{if(!found) print 0}'
}

detail "deploy start tag=${TAG} force=${FORCE:-no}"

# ── 세션 게이트: 방송 중 재시작 방지 ─────────────────────────────
if [[ "${FORCE}" != "--force" ]]; then
  for _ in $(seq 1 20); do
    sessions="$(active_sessions)"
    if [[ "${sessions}" == "0" ]]; then
      break
    fi
    echo "GATE: waiting (${sessions} sessions)"
    detail "gate wait sessions=${sessions}"
    sleep 30
  done
  sessions="$(active_sessions)"
  if [[ "${sessions}" != "0" ]]; then
    detail "gate timeout sessions=${sessions}"
    echo "DEPLOY DEFERRED"
    exit 75
  fi
fi

# ── 이미지 pull ──────────────────────────────────────────────────
docker login docker.io -u "${DOCKERHUB_USER}" --password-stdin \
  <"${TOKEN_FILE}" >>"${DETAIL_LOG}" 2>&1 || fail "docker login failed"
docker pull "${INNOLIVE_IMAGE}:${TAG}" >>"${DETAIL_LOG}" 2>&1 || fail "docker pull failed"
echo "PULL OK"

# ── 태그 교체(이전 태그는 롤백용으로 백업) ───────────────────────
if [[ -f "${TAG_FILE}" ]]; then
  cp "${TAG_FILE}" "${TAG_FILE}.prev"
fi
printf 'INNOLIVE_TAG=%s\n' "${TAG}" >"${TAG_FILE}"
chmod 644 "${TAG_FILE}"

# ── 재시작 + 헬스 확인 ───────────────────────────────────────────
systemctl restart innolive-server >>"${DETAIL_LOG}" 2>&1 || detail "systemctl restart exited nonzero"

deployed=false
for _ in $(seq 1 30); do
  sleep 3
  if health_ok; then
    deployed=true
    break
  fi
done

if [[ "${deployed}" == "true" ]]; then
  detail "deploy ok tag=${TAG}"
  echo "DEPLOY OK ${TAG}"
  exit 0
fi

# ── 실패 → 이전 태그 롤백 ────────────────────────────────────────
detail "health check failed, rolling back"
if [[ -f "${TAG_FILE}.prev" ]]; then
  cp "${TAG_FILE}.prev" "${TAG_FILE}"
  systemctl restart innolive-server >>"${DETAIL_LOG}" 2>&1 || detail "rollback restart exited nonzero"
  for _ in $(seq 1 30); do
    sleep 3
    if health_ok; then
      detail "rollback ok"
      echo "ROLLBACK OK"
      echo "FAILED: see server journal"
      exit 1
    fi
  done
fi

detail "rollback failed or no previous tag"
echo "MANUAL INTERVENTION REQUIRED"
exit 1
