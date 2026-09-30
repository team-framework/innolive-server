# 브랜치 생성

이슈를 만든 뒤 작업에 맞는 브랜치를 사용해요. 현재 작업 폴더와 `git worktree`를 모두 사용할 수 있어요.

## 형식

```text
<type>/<english-slug>/#<issue-number>
```

`type`은 `feat`, `fix`, `chore`, `refactor` 중 이슈와 같은 값을 사용해요. `english-slug`은 작업을 짧게 표현한 영문 케밥 케이스예요.

## 작업 위치

- 사용자가 현재 폴더나 특정 worktree를 지정했다면 그 위치를 사용해요.
- 현재 폴더에 관련 없는 변경이나 다른 작업이 있으면 그대로 두고 별도 worktree에서 진행해요.
- 이미 이 작업의 브랜치나 worktree가 있으면 재사용해요. 매번 기본 브랜치로 돌아가지 않아요.
- 기존 변경을 임의로 stash, restore, reset하거나 다른 작업의 worktree를 삭제하지 않아요.

## 새 브랜치 만들기

기본 브랜치와 시작 지점을 확인해요. 아래는 기본 브랜치가 `main`일 때의 예시예요.

현재 폴더가 깨끗하고 여기서 시작할 때:

```bash
git switch main
git pull --ff-only origin main
git switch -c feat/github-release-download/#654
```

별도 worktree에서 시작할 때:

```bash
git fetch origin main
git worktree add -b feat/github-release-download/#654 ../github-release-download-654 origin/main
```

사용자가 다른 시작 브랜치나 현재 변경 포함을 요청했다면 그 기준을 따라요. 위 예시의 브랜치명과 경로는 실제 작업에 맞춰 정해요.
