# Pull Request 생성

작업을 공유할 준비가 되면 PR을 초안(Draft)으로 먼저 만들어요. 처음부터 Open PR로 만들지 않아요.

## 제목

브랜치의 작업 유형은 콜론으로 구분하고, 나머지 슬러그와 이슈 번호는 브랜치와 같게 사용해요.

```text
<type>: <english-slug>/#<issue-number>
```

예시:

```text
fix: preflight-dim/#67
```

## 생성

변경 사항과 테스트를 확인하고 원격 브랜치를 푸시한 뒤 Draft PR을 만들어요.

```bash
git status --short
git push -u origin HEAD
gh pr create \
  --draft \
  --title "feat: github-release-download/#654" \
  --body "## 변경 내용\n- GitHub 릴리스 다운로드 기능을 추가했어요.\n\n## 확인 방법\n- 관련 테스트를 실행했어요.\n\nCloses #654"
```

## Open PR 전환

자가 확인이 끝난 뒤에만 Ready for review로 전환해요.

```bash
gh pr ready
```

Draft PR에는 리뷰 요청을 보내지 않아요. Ready for review로 전환되면 저장소의 CODEOWNERS 설정에 따라 담당 팀에 자동으로 리뷰가 요청돼요.

## 병합

`innolive-client`, `innolive-server`에서는 `main`에 직접 푸시하지 않아요. 최소 한 명의 승인과 필수 검사를 모두 통과한 뒤에만 병합해요.
