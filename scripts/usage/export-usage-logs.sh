#!/usr/bin/env bash
# 실사용 기록(#266)에 필요한 서버 로그 이벤트만 파일(JSON Lines)로 뽑는다.
#
# 세션·송출의 시작/종료, 라이브 전환, 일시정지/재개만 남기고, 필드도 기록에 쓰는
# 것만 고른다. 사용자 IP(ICE 후보)와 스트림 키는 애초에 담기지 않는다 — 송출 URL은
# 플랫폼 판별용으로 호스트까지만 남긴다. 결과는 그대로 cmd/usage-backfill에 넣을 수 있다.
#
# 사용법
#   로컬에서(SSH로 가져오기): INNOLIVE_SSH='-p <포트> <계정>@<서버>' scripts/usage/export-usage-logs.sh [출력 파일]
#   서버에서 직접:            scripts/usage/export-usage-logs.sh [출력 파일]
#
# 출력 파일을 생략하면 usage-events-<날짜-시각>.jsonl 로 저장한다.
set -euo pipefail

out="${1:-usage-events-$(date +%Y%m%d-%H%M).jsonl}"

# cmd/usage-backfill이 읽는 메시지와 같은 목록이다. 한쪽을 바꾸면 다른 쪽도 맞춘다.
filter='
fromjson?
| select(.session_id != null)
| select(.msg as $m | [
    "created live session",
    "closed live session",
    "RTMP egress started",
    "RTMP egress stopped",
    "RTMP egress stopped after terminal recovery failure",
    "platform broadcast is live",
    "RTMP egress paused",
    "RTMP egress resumed"
  ] | index($m))
| {time, msg, session_id, user_id, provider, reason, stop_reason,
   url: (if .url then (.url | capture("^(?<host>[a-z]+://[^/]+)").host) else null end)}
| with_entries(select(.value != null))
'

journal=(journalctl -u innolive-server -o cat --no-pager)
if [[ -n "${INNOLIVE_SSH:-}" ]]; then
  read -ra ssh_args <<< "$INNOLIVE_SSH"
  ssh "${ssh_args[@]}" "${journal[*]}" | jq -cR "$filter" > "$out"
else
  "${journal[@]}" | jq -cR "$filter" > "$out"
fi

echo "saved $(wc -l < "$out" | tr -d ' ') events to $out" >&2
