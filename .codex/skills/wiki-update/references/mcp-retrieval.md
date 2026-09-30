# MCP로 근거 찾기

`get_context`를 질문별 후보 섹션 조회에 먼저 쓴다. 보통 `max_chars: 12000`, `limit: 8`이면 충분하다. 검색 preview는 후보 선택용이다. 결과가 없거나 근거가 약하면 핵심 개념을 나누고 용어·동의어를 바꿔 제한적으로 다시 검색한다. 복합 질문은 사실 질문으로 나눠 조회한다. 결론은 `evidence[].content`와 원문 출처에서 뒷받침한다.

```json
{"query":"AI 서버 재시작 검증","max_chars":12000,"limit":8}
```

확인할 문서가 정해졌으면 `get_note_outline(path)`로 섹션 ID·hash·원본 줄을 얻고, `read_sections`에 해당 `path`, `section_id`, `hash`를 지정한다. 필요한 단락만 읽고, 인접 문단은 질문에 영향을 줄 때 추가한다. 링크는 결론의 전제·충돌·대체 정본을 확인해야 할 때만 따라간다. 질문이 문서 관계 전체를 묻는 경우에는 요청된 범위에서 탐색한다.

- `truncated: true`면 결과가 완전하지 않다. 응답의 `next_cursor`를 `read_sections`에 그대로 전달하거나 예산을 늘린다. cursor는 24자 불투명 참조이며 10분·서버 재시작·보관 한도 후 만료한다. 해석·장기 저장하지 않는다. 만료되면 같은 질문을 다시 조회한다.
- 표와 fenced code는 중간에서 잘리지 않는다. `required_chars`가 있으면 필요한 예산을 확인한다. 최대 `max_chars`는 128,000이다.
- `content_hash`/`hash`는 전체 섹션 hash다. 일부만 읽은 조각의 hash로 바꾸어 patch하지 않는다. 새 outline이나 검색 결과의 현재 hash를 사용한다.
- `get_context`의 필터는 `domain`, `owner`, `verification`, `include_history`다. 사건 기록은 기본 검색에서 제외한다. 과거 사실을 확인할 때만 `include_history`를 켠다.
- MCP 환경에 새 도구가 없으면 `search_wiki`로 후보를 찾고, 필요한 문서만 `read_note`로 연다. 전체 노트는 사용자가 전체 문서나 링크 관계를 요구할 때만 읽는다. 새 도구가 일부만 연결돼 있으면 사용할 수 있는 조회 도구와 범위를 설명한다.
- 변동 수치는 `get_current_metrics`로 확인한다. 검색 결과가 최근 머지를 반영하지 않으면 그 사실을 밝히고, MCP가 제공하지 않는 내용은 로컬 파일로 위장해 인용하지 않는다.

`get_context`는 Hybrid 검색(Qdrant 의미 검색 + lexical 검색)을 통해 후보를 찾는다. 의미 검색 상태가 응답에 표시되면 상태를 그대로 반영한다. 검색 순위 자체를 사실 근거로 보지 않는다.
