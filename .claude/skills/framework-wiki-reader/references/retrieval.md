# 조회와 근거

`get_context`로 질문별 근거를 찾는다. `domain`, `owner`, `verification`, `include_history`로 좁힐 수 있고 사건 기록은 기본 제외다. `evidence[].content`, `path`, `section_id`, `verification`, `source`, hash를 확인한다. preview와 순위는 원문 근거가 아니다.

근거가 약하거나 결과가 없으면 복합 질문을 나누고 핵심 용어·동의어·약어·영문 기술명으로 다시 조회한다. 그래도 못 찾으면 “조회한 범위에서 근거를 찾지 못했다”고 답한다. 위키 전체에 내용이 없다고 단정하지 않는다.

세부 내용은 `get_note_outline(path)`로 섹션 ID/hash를 얻고 `read_sections`에서 필요한 섹션만 읽는다. 링크는 전제·충돌·대체 정본을 확인할 때만 탐색한다. 전체 링크 조사는 사용자가 요청한 경우에 한다. 새 도구가 없으면 `search_wiki`로 후보를 찾고 선택한 문서만 `read_note`로 연다.

`truncated: true`는 근거 일부가 빠졌다는 뜻이다. `next_cursor`를 `read_sections`에 그대로 전달한다. cursor는 24자의 불투명 참조로, 10분 후·서버 재시작·보관 한도 초과 시 만료한다. 해석·저장하지 말고 만료되면 다시 검색한다. `max_chars`는 evidence JSON 문자 수이며 기본 12,000, 최대 128,000이다. 계속 잘리거나 도구가 없으면 읽은 범위와 미확인을 밝힌다.

`partial`, `chat-derived`, `unverified`는 확인된 사실과 분리한다. `runtime-verified`도 오래된 `as_of`면 현행으로 단정하지 않는다. 출처·verification 충돌이 남으면 `owner`와 근거를 보고 결론을 제한한다. 독립된 문서 갈래나 중요한 충돌이 있을 때만 작업을 분리한다.
