# 편집과 검토

원격 MCP는 읽기 전용이다. 수정은 로컬 Git checkout에 적용하고 Git PR로 공유한다. 범위가 작은 기존 문서는 필요한 섹션만 교체한다.

## Hash 기반 patch

먼저 `get_note_outline(path)`에서 현재 `section_id`, 섹션 `hash`, 전체 `note_hash`를 얻는다. 제안할 섹션은 `read_sections`로 확인한다. 요청 JSON은 다음과 같다.

```json
{
  "path":"운영/배포.md",
  "expected_note_hash":"현재 문서 SHA256",
  "operations":[{
    "section_id":"배포 권한:1",
    "expected_hash":"현재 섹션 SHA256",
    "replacement":"## 배포 권한\n승인된 계정으로 실행한다.\n\n"
  }]
}
```

`replacement`에는 제목을 포함한 섹션 전체를 넣는다. 로컬 도구는 `expected_note_hash`와 섹션 hash를 모두 요구해 다른 편집 뒤 적용을 막는다. 중복 섹션 요청, 오래된 hash, 경로 탈출, symlink 경로, Git checkout 바깥 파일은 거부한다.

저장소에 설치할 패키지는 없다. 표준 Python 3만으로 실행한다.

```sh
python3 /path/to/wiki-update/scripts/patch_note.py \
  --root /local/framework-llm-wiki --input /tmp/wiki-patch.json
python3 /path/to/wiki-update/scripts/patch_note.py \
  --root /local/framework-llm-wiki --input /tmp/wiki-patch.json --apply
```

기본 실행은 dry-run이며 hash와 compact diff를 출력한다. `diff_truncated: true`면 Git diff로 전체 변경을 확인한다. 실제 반영은 `--apply`를 지정했을 때만 한다. CLI는 Git lock으로 다른 CLI 실행을 막고 적용 직전에 문서 hash를 다시 확인해 임시 파일에서 원자적으로 교체한다. 다른 편집기는 이 lock을 쓰지 않으므로 마지막 Git diff도 확인한다. 남은 lock은 프로세스가 실행 중인지 확인한 뒤 처리한다.

새 문서는 `{"path":"...md","create":"Markdown 전체"}`를 사용한다. CLI는 기존 파일 덮어쓰기를 거부하고 첫 줄 YAML frontmatter의 `domain`, `question`, `owner`, `verification`을 확인한다. 새 문서 본문 토큰은 그대로 필요하다. 이 도구는 기존 문서를 다시 보내지 않는 데서 절감한다.

## 최종 검토

- 사실과 가설, 확인 방법과 결과를 분리한다. 코드 검토만 했다면 런타임 검증으로 쓰지 않는다.
- 삭제는 링크·참조 영향을 먼저 확인한다. 요청에 없던 삭제는 하지 않는다.
- `[[위키 링크]]`가 실제 대상 문서와 맞는지 확인한다.
- 개인 합의·개인 경로·자격 증명·불필요한 개인정보를 제거한다. 과거 상태에는 `[당시]`를 붙인다.
- 배포 태그·환경 수치처럼 바뀌는 값을 복제하지 않고 `_현행_수치.md`로 연결한다.
- 요청하지 않은 문서나 팀 합의를 더하지 않는다.
