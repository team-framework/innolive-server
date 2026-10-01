# Issue·Commit·PR 작성 규칙

## 제목과 브랜치

| 항목 | 형식 |
| --- | --- |
| Issue | `<type>: <한국어 작업 내용>` |
| 브랜치 | `<type>/<english-slug>/#<issue-number>` |
| Commit | `<type>: <한국어 변경 내용>` |
| PR | `<type>: <english-slug>/#<issue-number>` |
| 머지 커밋 | `merge: <head 브랜치 전체 이름>` |

Issue·브랜치·PR 타입은 `feat/fix/chore/refactor` 사용. 개별 커밋은 변경 목적에 따라 `feat/fix/refactor/hotfix/docs/style/remove/test/chore/comment` 사용. 기능 브랜치의 수정·문서·테스트 커밋은 각각 `fix/docs/test` 허용.

## 본문 문체

- 한 항목에 한 가지 내용, 최상위 `-` 개조식 사용
- 항목 끝은 `추가`, `수정`, `제거`, `통과`, `확인`, `대기` 등 명사형 사용
- `했습니다`, `합니다`, `했어요` 등 문장형 종결과 서술형 문단 사용 금지
- PR의 변경 내용·검증 결과는 각각 1~3개 항목
- PR에 구현 과정·작업 순서·시도한 방법 기록 금지
- 상세 근거·로그·성능 표는 Issue 또는 문서에 기록
- API 계약 영향·배포 선행 조건·중요한 미검증 범위는 핵심 항목으로 유지
- 중첩 목록·추가 PR 섹션·생성 도구 홍보 문구·em dash 사용 금지

## Issue

```markdown
## 작업 내용

- 해결할 문제 또는 추가할 기능

## 작업 체크리스트

- [ ] 구현 항목
- [ ] 변경 범위에 맞는 검증

## 완료 기준

- 완료 여부를 확인할 수 있는 동작 또는 결과
```

필요한 경우 마지막에 `## 참고`와 링크 목록 추가. 빈 참고 섹션 생략. 작업 체크리스트는 진행에 따라 갱신하며, 미완료 항목 유지 허용. 완료 기준은 작업 순서 대신 검증 가능한 결과로 작성. 기본 담당자는 실제 작업자 지정.

## PR

```markdown
## 변경 내용

- 카메라 중복 세션 생성 차단

## 검증 결과

- 카메라 세션 테스트 18개 통과
- 실제 기기 검증 대기

## 머지 체크리스트

- [x] 변경 범위 확인
- [x] 관련 테스트 완료
- [ ] 실제 기기 반복 진입 확인

## 관련 이슈

- Closes #395
```

- 구현 중 Draft 생성 허용. 검증 전에는 `검증 대기`와 미완료 체크박스 기재
- 체크리스트는 해당 변경의 머지에 필요한 항목만 작성. 변경 범위 확인과 검증 항목 포함
- `변경 범위 확인`은 고정 필수 항목. 해당 변경의 검증 항목을 하나 이상 추가
- 수행하지 않은 검증 체크 금지. 빌드·실기기 검증이 미대상이면 결과에 이유 기재
- 검증 결과는 실행 명령·대상·통과 개수·핵심 결과로 요약
- 검증 후 결과·체크 상태 갱신, Ready for review 전환 후 머지
- 리뷰는 선택사항. 승인 리뷰·리뷰 체크·미해결 대화를 공통 머지 조건으로 요구하지 않음
- `Closes #번호`는 이슈 전체 완료, `Refs #번호`는 일부 완료 또는 참고에 사용
- 중요 미검증 범위를 후속 Issue로 분리한 경우 `검증 결과`에 범위와 번호 기재. 필수 항목을 삭제해 검사만 통과시키지 않음

웹 편집 또는 `gh pr edit --body-file <파일>`로 체크 상태 갱신. 관련 이슈 키워드는 bare line 또는 `-` 항목 허용.

## 자동 동기화·배포 묶음 예외

- 하네스 Bot의 `harness-sync/framework-agent` 브랜치와 `chore: framework-agent-harness-sync` 제목 유지
- 자동 동기화도 공통 PR 본문·미완료 검증·머지 체크리스트 사용. 연결 이슈가 없으면 `- 관련 이슈 없음: 자동 하네스 동기화` 기재
- 배포 묶음은 `chore/batch-deploy-MMDD-N` 브랜치와 변환한 PR 제목 사용
- 배포 묶음의 관련 이슈는 `Refs`로 기재. 기능 PR이 완료한 이슈를 중복 종료하지 않음

## 자동 검사와 적용 시점

- `Collaboration / PR format`: 제목·브랜치·섹션 순서·개조식·문장형 종결·항목 수·이슈 연결 및 연결 Issue 본문 검사
- `Collaboration / merge readiness`: Draft의 미완료 검증 체크 허용, Ready PR의 미완료 검증 체크 차단
- 기존 PR 본문의 `리뷰 반영 및 미해결 의견 확인` 체크는 형식·머지 검사에서 제외. 검증 항목으로 계산하지 않음
- `Collaboration / policy tests`: 검사기의 정상·거부·Draft·Ready 전환 회귀 검증
- `Collaboration / Issue format`: 새 Issue의 본문 검사, 오류 시 `needs-format` 표시, 수정 후 표시 제거
- CI는 내용의 사실성·검증 충분성·과정 서술의 의미를 판정하지 않음. 작성자가 확인
- `.github/collaboration-policy.json`의 기존 번호 이하 기록은 새 양식 소급 적용 제외
- 적용 기준 파일은 저장소별 관리. 하네스 동기화 대상에서 제외하여 재동기화 시 기준 유지

## 병합

merge commit 사용, squash·rebase 비활성화. 병합 제목은 PR head 브랜치 전체 이름, 본문은 빈 값 지정.

GitHub 기본 설정은 PR 제목·빈 본문 조합 사용. 웹의 기본 제목은 `merge: <head 브랜치>`를 지원하지 않으므로 아래 명령 또는 웹 제목 편집으로 직접 지정.

```bash
head_branch=$(gh pr view --json headRefName --jq .headRefName)
head_sha=$(gh pr view --json headRefOid --jq .headRefOid)
gh pr checks --required
gh pr merge --merge --match-head-commit "$head_sha" \
  --subject "merge: $head_branch" --body ""
```

필수 검사와 검증 체크 완료 후 병합. GitHub 웹에서도 같은 제목·빈 본문 사용. 병합 후 실제 커밋의 제목·본문·두 부모 확인. 이 명령 예시는 사용자 승인·저장소 권한·요청 범위를 확장하지 않음.

## 규칙 원본

공통 문서·템플릿·검사기·작성 스킬 원본은 `team-framework/framework-agent-harness-sync`. MCP는 자동 동기화 제외를 유지하고 같은 변경을 별도 PR로 적용. 저장소 고유 지침과 관련 없는 작업 보존.

현재 하네스 App 권한은 Contents·PR 쓰기이며 Workflows 쓰기 권한은 없음. 검사 workflow의 최초 설치와 workflow 자체 변경은 관리자 PR로 별도 적용. 자동 동기화는 문서·템플릿·스킬·검사기 파일을 전파하며 workflow 파일을 작성하지 않음. 새 저장소에는 `manualBootstrapItems`의 workflow와 저장소별 적용 기준·필수 검사 설정을 먼저 적용.

연결 Issue 양식 수정 후 PR 검사가 실패 상태라면 해당 PR 검사를 재실행하거나 PR 본문을 갱신하여 최신 Issue를 검사.
