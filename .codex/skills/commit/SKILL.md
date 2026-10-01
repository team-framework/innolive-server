---
name: commit
description: Framework 저장소의 목적별 커밋 작성 시 한국어 제목과 변경 종류에 맞는 타입 적용.
---

# Commit 작성

[공통 작성 규칙](../../../docs/collaboration.md)의 커밋 타입을 따른다.

- 형식은 `<type>: <한국어 변경 내용>`
- 타입은 `feat/fix/refactor/hotfix/docs/style/remove/test/chore/comment`
- 한 커밋에 한 가지 목적. 개별 커밋 타입은 Issue·브랜치 타입과 달라도 됨
- 기능 브랜치에서도 오류 수정은 `fix`, 문서는 `docs`, 테스트는 `test`로 구분
- 코드 식별자와 전문 용어는 실제 표기 유지
- 커밋 전 변경 파일·검증 범위·`git diff --check` 확인
- 관련 없는 파일·비밀 정보·생성물 제외

```bash
git status --short
git diff --check
git add <변경한-파일>
git commit -m "fix: 얼굴 등록 카메라 세션 충돌 수정"
```
