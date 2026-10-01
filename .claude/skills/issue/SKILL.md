---
name: issue
description: Framework 저장소의 Issue 생성 및 작업 체크리스트 갱신 시 공통 제목과 개조식 본문 적용.
---

# Issue 작성

[공통 작성 규칙](../../../docs/collaboration.md)의 Issue 양식을 읽는다.

- 작업 전 Issue 생성, 실제 작업자 지정
- 제목은 `feat/fix/chore/refactor: <한국어 작업 내용>`
- 본문은 `작업 내용 / 작업 체크리스트 / 완료 기준`, 필요한 경우 마지막에 `참고` 추가
- 한 항목에 한 가지 내용, 명사형 종결 사용. 진행 항목은 `- [ ]`로 작성하고 수행 후 체크
- 완료 기준은 확인 가능한 결과로 작성
- CLI 본문은 UTF-8 Markdown 파일과 `--body-file` 사용. 빈 본문과 문자열 `\n` 사용 금지

```bash
gh issue create --title "fix: 카메라 세션 충돌 수정" \
  --label "fix" --assignee "@me" --body-file issue.md
```

생성한 번호를 브랜치와 PR에 사용한다. 작업 진행에 따라 기존 Issue 체크 상태를 갱신한다.
