#!/usr/bin/env bash
# GitHub Secrets → server-secrets.env 렌더러.
# receive-deploy.sh의 "sync-config" verb가 sudo로 실행하며, stdin으로 KEY=VALUE 본문을 받는다.
#
# 출력 계약: stdout/stderr는 GitHub Actions 러너 로그로 그대로 전달되어 공개된다.
# 요약 키워드와 키 "이름" 외에는 아무것도(특히 값·서버 상세) 출력하지 말 것.
# 상세 진단은 로그 디렉토리(600) 파일에만 기록한다.
set -euo pipefail

SECRETS_FILE="${INNOLIVE_SECRETS_FILE:-/etc/innolive/server-secrets.env}"
LOCK_FILE="${INNOLIVE_SYNC_LOCK:-/opt/innolive/deploy/.sync-config.lock}"
LOG_DIR="${INNOLIVE_DEPLOY_LOG_DIR:-/opt/innolive/deploy/logs}"
HEALTH_URL="${INNOLIVE_HEALTH_URL:-http://127.0.0.1:8000/health}"
METRICS_URL="${INNOLIVE_METRICS_URL:-http://127.0.0.1:8000/metrics}"
SERVICE="${INNOLIVE_SERVICE:-innolive-server.service}"
GATE_ATTEMPTS="${INNOLIVE_GATE_ATTEMPTS:-20}"
GATE_INTERVAL="${INNOLIVE_GATE_INTERVAL:-30}"

# 렌더 허용 키. REQUIRED는 비어 있으면 거부, OPTIONAL은 빈 값 허용.
# AUTH_EMAIL_REDIS_PASSWORD: 프로덕션 Redis가 로컬 전용·무인증 구성이면 빈 값이 정상이므로 OPTIONAL.
# YOUTUBE_STREAM_KEY: 빈 값이면 서버가 유튜브 송출을 건너뛰는 기존 거동을 그대로 따른다.
REQUIRED_KEYS="DATABASE_URL AUTH_ACCESS_TOKEN_KEY_BASE64 AUTH_PROVIDER_TOKEN_ENCRYPTION_KEY_BASE64 AUTH_EMAIL_SMTP_PASSWORD WEBRTC_TURN_USERNAME WEBRTC_TURN_CREDENTIAL APPLE_TEAM_ID APPLE_KEY_ID"
OPTIONAL_KEYS="YOUTUBE_STREAM_KEY AUTH_EMAIL_REDIS_PASSWORD"

DRY_RUN=false
NO_RESTART=false
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    --no-restart) NO_RESTART=true ;;
    *) echo "FAILED: unknown argument"; exit 1 ;;
  esac
done

mkdir -p "$LOG_DIR"; chmod 700 "$LOG_DIR"
DETAIL_LOG="$LOG_DIR/sync-$(date +%Y%m%d-%H%M%S).log"
touch "$DETAIL_LOG"; chmod 600 "$DETAIL_LOG"
log() { printf '%s %s\n' "$(date '+%F %T')" "$*" >>"$DETAIL_LOG"; }

in_list() { # $1=key $2=공백구분 목록
  case " $2 " in *" $1 "*) return 0 ;; *) return 1 ;; esac
}

# ── stdin 파싱·검증 (실패 시 어떤 파일도 건드리지 않는다) ─────────────
KEYS=()
VALUES=()
SEEN=""
line_no=0
while IFS= read -r line || [ -n "$line" ]; do
  line_no=$((line_no + 1))
  line="${line%$'\r'}"
  if [ -z "$line" ]; then
    echo "FAILED: invalid input (line $line_no)"; log "empty line $line_no"; exit 1
  fi
  key="${line%%=*}"
  value="${line#*=}"
  if [ "$key" = "$line" ] || ! printf '%s' "$key" | grep -Eq '^[A-Z][A-Z0-9_]*$'; then
    echo "FAILED: invalid input (line $line_no)"; log "malformed line $line_no"; exit 1
  fi
  if ! in_list "$key" "$REQUIRED_KEYS $OPTIONAL_KEYS"; then
    echo "FAILED: key not allowed: $key"; log "unknown key $key"; exit 1
  fi
  if in_list "$key" "$SEEN"; then
    echo "FAILED: duplicate key: $key"; log "duplicate key $key"; exit 1
  fi
  if in_list "$key" "$REQUIRED_KEYS" && [ -z "$value" ]; then
    echo "FAILED: required key empty: $key"; log "empty required $key"; exit 1
  fi
  KEYS+=("$key"); VALUES+=("$value"); SEEN="$SEEN $key"
done

for key in $REQUIRED_KEYS $OPTIONAL_KEYS; do
  if ! in_list "$key" "$SEEN"; then
    echo "FAILED: key missing: $key"; log "missing key $key"; exit 1
  fi
done

lookup() { # $1=key → 대응 값 출력
  local i=0
  while [ $i -lt ${#KEYS[@]} ]; do
    if [ "${KEYS[$i]}" = "$1" ]; then printf '%s' "${VALUES[$i]}"; return 0; fi
    i=$((i + 1))
  done
  return 1
}

current_value() { # $1=key → 현행 파일의 값(없으면 rc 1)
  [ -f "$SECRETS_FILE" ] || return 1
  grep -m1 "^$1=" "$SECRETS_FILE" | cut -d= -f2- || return 1
}

# ── dry-run: 키 이름 수준 diff만 보고 ────────────────────────────────
if $DRY_RUN; then
  changed=""; added=""
  for key in $REQUIRED_KEYS $OPTIONAL_KEYS; do
    if old="$(current_value "$key")"; then
      [ "$old" = "$(lookup "$key")" ] || changed="$changed $key"
    else
      added="$added $key"
    fi
  done
  removed=""
  if [ -f "$SECRETS_FILE" ]; then
    while IFS= read -r existing_key; do
      in_list "$existing_key" "$REQUIRED_KEYS $OPTIONAL_KEYS" || removed="$removed $existing_key"
    done < <(grep -E '^[A-Z][A-Z0-9_]*=' "$SECRETS_FILE" | cut -d= -f1)
  fi
  echo "SYNC DRY-RUN changed:[${changed# }] added:[${added# }] removed:[${removed# }]"
  log "dry-run changed=$changed added=$added removed=$removed"
  exit 0
fi

# ── 잠금 ────────────────────────────────────────────────────────────
# flock이 없는 환경(로컬 macOS 테스트)에서는 잠금을 생략한다 — 서버(Linux)에는 항상 존재.
if command -v flock >/dev/null 2>&1; then
  exec 9>"$LOCK_FILE"
  if ! flock -n 9; then
    echo "FAILED: another sync in progress"; log "flock busy"; exit 1
  fi
fi

# ── 세션 게이트 (재시작 예정일 때만, 교체 전에 수행) ──────────────────
active_sessions() {
  curl -fsS --max-time 5 "$METRICS_URL" 2>/dev/null \
    | awk '/^innolive_active_sessions[ {]/{print $NF; found=1} END{if(!found) print 0}' \
    | head -1
}
if ! $NO_RESTART; then
  attempt=0
  while :; do
    sessions="$(active_sessions)"
    case "$sessions" in ''|*[!0-9.]*) sessions=0 ;; esac
    if [ "${sessions%%.*}" -eq 0 ]; then break; fi
    attempt=$((attempt + 1))
    if [ "$attempt" -ge "$GATE_ATTEMPTS" ]; then
      echo "SYNC DEFERRED"; log "gate timeout sessions=$sessions"; exit 75
    fi
    log "gate wait attempt=$attempt sessions=$sessions"
    sleep "$GATE_INTERVAL"
  done
fi

# ── 원자 교체 + 백업 ─────────────────────────────────────────────────
umask 077
tmp="$(mktemp "${SECRETS_FILE}.tmp.XXXXXX")"
{
  echo "# sync-config 렌더 산출물 — 손편집 금지(모든 변경은 GitHub Secrets 경유)"
  echo "# rendered: $(date '+%F %T')"
  for key in $REQUIRED_KEYS $OPTIONAL_KEYS; do
    printf '%s=%s\n' "$key" "$(lookup "$key")"
  done
} >"$tmp"
chmod 600 "$tmp"
BAK=""
if [ -f "$SECRETS_FILE" ]; then
  BAK="${SECRETS_FILE}.bak-$(date +%Y%m%d-%H%M%S)"
  cp -p "$SECRETS_FILE" "$BAK"
fi
mv -f "$tmp" "$SECRETS_FILE"
log "rendered ok bak=${BAK:-none}"

key_count=$(( $(printf '%s' "$REQUIRED_KEYS $OPTIONAL_KEYS" | wc -w) ))

if $NO_RESTART; then
  echo "SYNC OK ($key_count keys, restart skipped — applies on next restart)"
  exit 0
fi

# ── 재시작 + 헬스 확인, 실패 시 복원 ─────────────────────────────────
systemctl restart "$SERVICE"
healthy() { curl -fsS --max-time 3 "$HEALTH_URL" >/dev/null 2>&1; }
i=0
while [ $i -lt 30 ]; do
  sleep 3
  if healthy; then echo "SYNC OK ($key_count keys)"; log "health ok"; exit 0; fi
  i=$((i + 1))
done
log "health failed — restoring backup"
if [ -n "$BAK" ] && [ -f "$BAK" ]; then
  cp -p "$BAK" "$SECRETS_FILE"
  systemctl restart "$SERVICE"
  i=0
  while [ $i -lt 30 ]; do
    sleep 3
    if healthy; then log "rollback health ok"; break; fi
    i=$((i + 1))
  done
fi
echo "FAILED: see server journal"
exit 1
