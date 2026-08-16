# 커밋 작성

커밋은 한 가지 목적의 변경만 담고, 제목은 한국어로 작성해요.

## 형식

```text
<type>: <변경 내용>
```

`type`은 `feat`, `fix`, `chore`, `refactor` 중 하나이며, 이슈 및 브랜치의 유형과 맞춰요.

예시:

```text
feat: 다운로드 버튼과 진행 상태 추가
fix: 로그인 후 잘못된 경로 이동 수정
chore: 테스트 의존성 업데이트
refactor: 인증 토큰 검증 로직 분리
```

## 확인 후 커밋

변경 파일과 테스트 결과를 확인한 뒤 커밋해요.

```bash
git status --short
git diff --check
git add <변경한-파일>
git commit -m "feat: 다운로드 버튼과 진행 상태 추가"
```

관련 없는 파일, 비밀 정보, 생성물은 커밋하지 않아요.
