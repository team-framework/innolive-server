#!/usr/bin/env bash
# 서버 런타임 로그(journald의 innolive-server)를 날짜별 파일로 보관한다.
# 사용법: log-archive.sh   (root. 매일 innolive-log-archive.timer와 배포 직전 apply-release.sh가 호출)
#
# journald는 용량 상한에 닿으면 오래된 로그부터 지우므로, 보관 디스크에 하루 한 파일
# (innolive-server-YYYY-MM-DD.log.zst, 서버 현지 날짜)로 옮겨 둔다.
# - 아직 파일이 없는 날은 채우고, 어제와 오늘은 매번 다시 쓴다(멱등). 첫 실행은
#   journald에 남은 가장 오래된 날부터 채운다.
# - 스트림 키·토큰·비밀번호는 가려서 쓴다. 사용자 IP(ICE 후보)는 장애 조사에 필요해 남긴다.
# - 출력은 요약 한 줄뿐이다. 로그 본문을 stdout으로 내보내지 않는다(배포 출력 계약).
set -euo pipefail

ARCHIVE_DIR="${INNOLIVE_LOG_ARCHIVE_DIR:-/srv/innolive-logs}"
UNIT="innolive-server"

# 보관 디스크가 빠진 상태에서 루트 디스크의 빈 마운트 지점에 쓰는 사고를 막는다.
if ! mountpoint -q "${ARCHIVE_DIR}"; then
  echo "log-archive: ${ARCHIVE_DIR} is not a mount point; nothing written" >&2
  exit 1
fi

mask() {
  sed -E \
    -e 's#(rtmps?://[^ "]*/)[^ "/]+#\1***MASKED***#g' \
    -e 's#((stream_?key|access_?token|refresh_?token|owner_?token|client_?secret|secret|password|authorization)"?[=:] ?"?)[A-Za-z0-9._~+/=-]{16,}#\1***MASKED***#Ig' \
    -e 's#(Bearer )[A-Za-z0-9._~+/=-]+#\1***MASKED***#g'
}

# head가 먼저 끝나 journalctl이 SIGPIPE를 받는 것은 정상이다 — 값은 아래에서 검증한다.
first_day="$(journalctl -u "${UNIT}" -o short-iso --no-pager 2>/dev/null | head -1 | cut -c1-10 || true)"
if [[ ! "${first_day}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
  echo "log-archive: no journal entries for ${UNIT}" >&2
  exit 0
fi

# 중간에 실패하면 쓰다 만 임시 파일을 지운다.
tmp=""
trap 'rm -f "${tmp}"' EXIT

today="$(date +%F)"
yesterday="$(date -d yesterday +%F)"
written=0
day="${first_day}"
while [[ ! "${day}" > "${today}" ]]; do
  next="$(date -d "${day} +1 day" +%F)"
  out="${ARCHIVE_DIR}/${UNIT}-${day}.log.zst"
  if [[ ! -e "${out}" || ! "${day}" < "${yesterday}" ]]; then
    tmp="$(mktemp "${ARCHIVE_DIR}/.${UNIT}-${day}.XXXXXX")"
    journalctl -q -u "${UNIT}" -o short-iso --no-pager --since "${day} 00:00:00" --until "${next} 00:00:00" \
      | mask | zstd -q -19 -f -o "${tmp}"
    chgrp adm "${tmp}"
    chmod 640 "${tmp}"
    mv -f "${tmp}" "${out}"
    written=$((written + 1))
  fi
  day="${next}"
done

echo "log-archive: ${written} day file(s) written to ${ARCHIVE_DIR} (${first_day}..${today})"
