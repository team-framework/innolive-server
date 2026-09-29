---
name: wiki-update
description: InnoLive 팀 LLM 위키에 확인된 세션 사실을 반영하고 검증한 뒤 요청 범위에 맞춰 커밋과 PR까지 진행한다. 위키 문서화·업데이트 요청에 사용한다. 위키 검색·질문이나 코드 레포 수정에는 사용하지 않는다.
license: 팀 내부용
metadata:
  version: 0.9.0
  owner: server-team
  repo: team-framework/framework-llm-wiki
  mcp-server: framework-wiki
---

# Wiki Update

위키 Markdown은 사람이 읽는 정본이고 MCP는 읽기·검색 창구다. 수정은 지정된 위키 Git checkout에서 한다. 기존 사실은 근거 문서로 대조하고, 세션에서 나온 사실·과거 사실·가설·할 일을 구분한다.

## 진행

1. 클론 경로를 모르면 한 번 묻는다. 원격과 Git 상태를 확인한다. 기존 변경은 보존하고, 깨끗한 별도 worktree에서 요청 범위만 작업한다.
2. `get_wiki_status`와 로컬 HEAD를 비교한다. `get_context`로 관련 문단을 찾고, 필요한 부분만 `get_note_outline`·`read_sections`로 읽는다. [MCP 조회 절차](references/mcp-retrieval.md)
3. 기존 `question`에 맞는 문서를 우선 갱신한다. 근거, 담당 `owner`, `verification`, 시점을 정리하고 요청 밖의 변경을 빼둔다. [내용·메타데이터 기준](references/metadata-and-dates.md)
4. 선택한 섹션만 고친다. 전체 문서·섹션 hash를 검증하는 로컬 patch CLI를 우선 사용하고, 사람이 diff를 확인한다. [편집·검토 절차](references/edit-review.md)
5. 문서 링크·개인정보·과거 시점·중복 수치와 Git diff를 확인한다. 검증이 끝나면 요청이 허용한 범위에서 커밋·push·PR을 진행하고, 머지하지 않는다. [Git·PR 절차](references/git-publish.md)

## 멈춤 조건

원격이 다른 위키이거나, 대상 브랜치가 분기했거나, 근거 충돌을 해결할 수 없으면 파일을 덮거나 사실을 추정하지 말고 상태와 필요한 정보만 보고한다. 사용자가 로컬 전용 등 범위를 제한하면 그 범위를 따른다.
