#!/usr/bin/env bash
# deploy 계정의 SSH forced command 진입점.
# authorized_keys의 command= 로만 실행되며, 허용 형식 외에는 어떤 명령도 수행하지 않는다.
#   deploy <sha> [--force]                  → apply-release.sh (배포)
#   sync-config [--dry-run] [--no-restart]  → render-secrets.sh (시크릿 렌더, 본문은 stdin)
#
# 출력 계약: 이 스크립트의 stdout/stderr는 GitHub Actions 러너 로그로 그대로 전달되어
# 공개 레포에서 누구나 볼 수 있다. 단계 키워드와 커밋 SHA 외에는 아무것도 출력하지 말 것.
set -euo pipefail

read -r command arg1 arg2 _extra <<<"${SSH_ORIGINAL_COMMAND:-}" || true

if [[ -n "${_extra:-}" ]]; then
  echo "DENIED"
  exit 1
fi

case "${command:-}" in
  deploy)
    # 태그는 git 커밋 SHA(7~40자리 소문자 16진수)만 허용한다.
    if [[ ! "${arg1:-}" =~ ^[0-9a-f]{7,40}$ ]]; then
      echo "DENIED"
      exit 1
    fi
    if [[ -n "${arg2:-}" && "${arg2}" != "--force" ]]; then
      echo "DENIED"
      exit 1
    fi
    exec sudo /opt/innolive/deploy/apply-release.sh "${arg1}" ${arg2:+--force}
    ;;
  sync-config)
    for flag in "${arg1:-}" "${arg2:-}"; do
      if [[ -n "$flag" && "$flag" != "--dry-run" && "$flag" != "--no-restart" ]]; then
        echo "DENIED"
        exit 1
      fi
    done
    if [[ -n "${arg1:-}" && "${arg1}" == "${arg2:-}" ]]; then
      echo "DENIED"
      exit 1
    fi
    exec sudo /opt/innolive/deploy/render-secrets.sh ${arg1:+"$arg1"} ${arg2:+"$arg2"}
    ;;
  *)
    echo "DENIED"
    exit 1
    ;;
esac
