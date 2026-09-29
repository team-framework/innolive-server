---
name: framework-wiki-reader
description: Framework·InnoLive 내부 지식을 위키 MCP에서 검색하고 원문 근거와 검증 상태를 확인해 답한다. 위키 검색·비교 질문에 사용한다.
---

# Framework Wiki Reader

`get_context`에서 질문에 필요한 evidence를 찾고 본문·경로·`verification`을 근거로 답한다. 근거가 부족하면 질문을 나누거나 용어·동의어를 바꿔 bounded 재검색한다.

`resolved_links`의 단일 경로를 우선하고, 없거나 모호하면 검색으로 확인한다. 필요한 상세만 `get_note_outline`·`read_sections`로 읽으며 링크 전수 조사는 요청 범위에서 한다. 잘린 결과는 완전하다고 표현하지 않는다. cursor는 그대로 이어 읽고, 만료되면 새로 검색한다.

사실마다 출처를 붙이고 충돌·오래된 관측·미확인을 밝힌다. 도구가 없을 때만 `search_wiki`와 필요한 `read_note`로 대체한다. [조회·근거 규칙](references/retrieval.md)
