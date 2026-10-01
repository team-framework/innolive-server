---
name: pull-request
description: Framework 저장소의 Draft PR 생성, 검증 결과와 체크리스트 갱신, merge commit 작성 시 공통 규칙 적용.
---

# PR 작성

[공통 작성 규칙](../../../docs/collaboration.md)과 `.github/pull_request_template.md`를 읽는다.

## 생성과 갱신

- 제목은 `<type>: <english-slug>/#<issue-number>`, head 브랜치와 일치
- 구현 중 Draft 생성 허용. 검증 전에는 결과에 `검증 대기`, 체크리스트에는 미완료 항목 기재
- 본문은 `변경 내용 / 검증 결과 / 머지 체크리스트 / 관련 이슈` 순서 고정
- 명사형 종결의 최상위 개조식 사용. 변경 내용·검증 결과는 각각 1~3개 항목
- 핵심 동작·영향·실제 결과만 기록. 구현 과정·작업 순서·시도한 방법·중첩 목록·홍보 문구 생략
- 중요한 미검증 범위와 API 계약·배포 조건 유지. 상세 설명은 Issue 또는 문서에 기록
- 체크리스트는 해당 변경의 필수 검증과 리뷰 확인으로 구성. 미수행 항목 체크 금지
- 실제 검증 후 결과·체크 상태 갱신, Ready for review 전환, 리뷰 반영 후 머지
- 이슈 전체 완료는 `Closes #번호`, 부분 완료·참고는 `Refs #번호`
- 본문은 파일과 `--body-file`로 전달. 문자열 `\n` 사용 금지

```bash
git push -u origin HEAD
gh pr create --draft --title "fix: camera-session/#123" --body-file pr.md
gh pr edit --body-file pr.md
gh pr ready
```

## 병합

사용자 요청 범위와 저장소 승인 조건을 확인한다. 공통 규칙의 필수 검사·체크리스트·리뷰 확인 후 merge commit으로 병합한다.

```bash
head_branch=$(gh pr view --json headRefName --jq .headRefName)
head_sha=$(gh pr view --json headRefOid --jq .headRefOid)
gh pr checks --required
gh pr merge --merge --match-head-commit "$head_sha" \
  --subject "merge: $head_branch" --body ""
```

병합 후 실제 커밋의 제목·빈 본문·두 부모를 확인한다. 자동 동기화·배포 묶음 PR의 예외는 공통 규칙을 따른다.
