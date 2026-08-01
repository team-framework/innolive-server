# 브랜치 생성

이슈를 만든 뒤, 실제 로컬 작업 폴더에서 브랜치를 만들어요. `git worktree`는 사용하지 않아요.

## 형식

```text
<type>/<english-slug>/#<issue-number>
```

`type`은 `feat`, `fix`, `chore`, `refactor` 중 이슈와 같은 값을 사용해요. `english-slug`은 작업을 짧게 표현한 영문 케밥 케이스예요.

예시:

```text
feat/github-release-download/#654
fix/login-redirect/#655
chore/dependency-update/#656
refactor/auth-service/#657
```

## 만들기

기본 브랜치를 최신 상태로 맞춘 뒤, 현재 작업 폴더에서 새 브랜치로 전환해요.

```bash
git switch main
git pull --ff-only origin main
git switch -c feat/github-release-download/#654
```

저장소의 기본 브랜치가 `main`이 아니라면 해당 브랜치 이름을 사용해요.
