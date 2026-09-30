# Checkout, commit, PR

## 대상과 기존 변경

제공된 경로에서 `git rev-parse --show-toplevel`, `git remote get-url origin`, `git status --short -uall`을 확인한다. origin은 `team-framework/framework-llm-wiki`여야 한다. Git checkout이 아니거나 다른 원격이면 중단한다.

변경이 있으면 출처를 브랜치명으로 추정하거나 reset·restore·clean·stash하지 않는다. 기존 worktree를 보존하고 요청을 위한 별도 `git worktree`를 만든다. 사용자가 이 worktree의 변경도 요청한 경우에만 그 근거를 읽어 요청 범위에 반영한다. 잠금 파일·기존 커밋은 임의로 삭제하지 않는다.

깨끗한 checkout이면 `main`을 `pull --ff-only`로 갱신한다. 로컬 커밋이 갈라져 fast-forward가 안 되면 reset하지 말고 중단한다. 이후 `get_wiki_status.wiki_commit`과 로컬 HEAD를 비교한다. MCP가 뒤처져도 로컬 checkout에서 본문을 추정해 인용하지 말고, 색인 지연을 답변에 밝힌다.

## 게시

위키 레포의 브랜치 관례와 GitHub 계정을 확인한다. 기본 브랜치에 직접 쓰지 않는다. 변경 파일만 stage하고 목적 하나의 한국어 커밋을 만든다. 관련 이슈가 대화·커밋에서 확인되면 전체 레포 경로로 연결한다. 위키 변경만을 위해 새 이슈를 만들 필요는 없다.

위키 업데이트 요청은 검증 후 push와 PR 생성을 포함한다. 사용자가 로컬 전용 등 제한을 주면 원격에 게시하지 않는다. 자동으로 머지하지 않는다. push가 실패하면 커밋과 브랜치를 보존하고 이유를 보고한다. push 후 PR을 만들 수 없으면 브랜치와 커밋 SHA, 남은 조치를 보고한다.

PR 생성 후 URL과 변경 문서를 보고한다. 머지 및 홈서버 재색인 전에는 원격 검색 결과가 바뀌었다고 말하지 않는다. 이후 `get_wiki_status`로 색인 commit을 확인한다.
