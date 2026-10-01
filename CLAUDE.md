<!-- framework-collaboration-harness:start -->
# Framework 협업 규칙

작업 종류와 사용하는 에이전트에 맞는 `.codex`, `.claude`, `.agent`의 `skills/{issue,branch,commit,pull-request}/SKILL.md`를 읽는다. Issue·Commit·PR 작성 시 `docs/collaboration.md`와 해당 템플릿을 따른다.

- Issue는 한국어 제목과 작업 체크리스트, 완료 기준을 작성한다.
- 브랜치는 `<type>/<english-slug>/#<issue-number>` 형식을 사용한다.
- 커밋은 변경 목적별로 분리하고 `<type>: <한국어 변경 내용>` 형식을 사용한다. 개별 커밋 타입은 브랜치와 달라도 된다.
- PR은 구현 중 Draft로 생성할 수 있다. 본문은 변경 내용·검증 결과·머지 체크리스트·관련 이슈의 개조식으로 작성한다.
- PR에 구현 과정과 서술형 문단을 적지 않는다. 변경 내용과 검증 결과는 각각 1~3개 항목으로 제한한다.
- 미검증 항목은 체크하지 않는다. 실제 검증 후 결과와 체크 상태를 갱신하고 필수 항목 완료 후 머지한다.
- merge commit만 사용하고 제목은 `merge: <head 브랜치 전체 이름>`, 본문은 빈 값으로 지정한다.
- 기존 Issue·PR 기록, 다른 사람의 변경, 저장소 고유 규칙을 보존한다.
<!-- framework-collaboration-harness:end -->
