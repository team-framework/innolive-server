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

PR 본문은 아래 틀을 유지해요.

```markdown
## 변경 내용

- 핵심 변경 사항
- 주요 동작 또는 영향 범위

## 확인 방법

- 빌드·테스트 결과
- 주요 검증 항목

Closes #<issue-number>
```

본문은 구현 과정을 길게 설명하지 않고 핵심 결론만 간결한 개조식으로 작성해요.

- 비슷한 변경은 한 항목으로 묶어요.
- 세부 구현보다 사용자가 체감하는 동작과 중요한 기술적 영향만 남겨요.
- 테스트 개수, 성공 여부, 핵심 검증 범위만 남겨요.
- signaling 변경 없음, 물리 기기 미검증처럼 중요한 제약·미수행 항목은 유지해요.
- 이미 본문에서 드러나는 내용을 반복하지 않아요.

```bash
git status --short
git push -u origin HEAD
gh pr create \
  --draft \
  --title "feat: github-release-download/#654" \
  --body "## 변경 내용\n\n- GitHub 릴리스 다운로드 기능 추가\n\n## 확인 방법\n\n- 관련 테스트 통과\n\nCloses #654"
```

## Open PR 전환

자가 확인이 끝난 뒤에만 Ready for review로 전환해요.

```bash
gh pr ready
```

## 병합

병합 커밋 제목은 GitHub 기본 제목을 사용하지 않고 아래 형식으로 작성해요.

```text
merge: <english-slug>/#<issue-number>
```

브랜치가 다음과 같다면:

```text
chore/readme-update/#390
```

기본 제목:

```text
Merge pull request #392 from team-framework/chore/readme-update/#390
```

대신 다음 제목을 사용해요.

```text
merge: chore/readme-update/#390
```

CLI로 병합할 때도 병합 커밋 제목을 직접 지정해요.

```bash
gh pr merge --merge \
  --subject "merge: chore/readme-update/#390"
```
