# InnoLive 배포 파이프라인 운영 가이드

main 브랜치에 머지되면 GitHub Actions가 Docker 이미지를 빌드해 팀 Docker Hub(private)에
push하고, GPU 프로덕션 서버가 이미지를 pull해 컨테이너를 재기동한다.

```
PR 머지 → main push → [CI: build-and-test(필수)] → [Deploy 워크플로]
  → docker build & push (docker.io/<팀계정>/innolive-server:<커밋SHA>)
  → SSH(deploy 계정, forced command) → 서버 apply-release.sh
  → 세션 게이트 → pull → systemd 재시작 → /health 확인 → 실패 시 이전 태그 롤백
```

## 배포 규칙 (단일 경로 원칙)

- **모든 배포는 main 머지로만 한다.** 서버에 SSH로 들어가 바이너리·소스를 직접 수정/빌드하지
  않는다. 손배포는 서버를 git과 무관한 상태(drift)로 만들어 디버깅을 불가능하게 한다.
- 마이그레이션은 backward-compatible하게 작성한다(롤백 시 구버전이 신 스키마 위에서 돈다).
- 워크플로·배포 스크립트를 수정하는 PR은 **로그 출력 변화**를 리뷰 항목에 포함한다
  (public 레포 — Actions 로그는 전 세계 공개).

## 긴급 상황 (공식 비상 경로)

- **활성 세션 때문에 배포가 보류됨** (`DEPLOY DEFERRED`): Actions → Deploy →
  Run workflow에서 `force_restart` 체크 후 실행 — 세션 게이트를 생략하고 즉시 재시작한다.
  방송이 끊기므로 정말 급할 때만.
- **잘못된 버전이 나감 → 특정 과거 커밋으로 롤백**: 서버에서
  `sudo /opt/innolive/deploy/apply-release.sh <과거커밋SHA> --force`
  (이미지가 레지스트리에 남아 있으므로 어떤 과거 커밋으로든 즉시 이동 가능).
- **배포 실패 원인 확인**: 러너 로그에는 의도적으로 요약만 남는다. 상세는 서버의
  `/opt/innolive/deploy/logs/`(root 600)와 `journalctl -u innolive-server`에서 본다.

## 서버 설치 절차 (1회, 이미 적용됨 — 재구축 시 참고)

전제: Docker, systemd 기반 innolive-ai@0/1, `/etc/innolive/server.env`·`server-secrets.env`.

1. **서버 로컬 설정 파일** (레포에 넣지 않는 값들):
   - `/etc/innolive/deploy.env` (root 600):
     ```
     DOCKERHUB_USER=<팀 Docker Hub 계정명>
     INNOLIVE_IMAGE=docker.io/<팀 Docker Hub 계정명>/innolive-server
     INNOLIVE_APPLE_KEY_PATH=<server-secrets.env의 APPLE_PRIVATE_KEY_PATH와 동일 경로>
     ```
   - `/etc/innolive/dockerhub.token` (root 600): Docker Hub **Read-only** Access Token.
2. **배포 스크립트 설치**: 이 디렉토리의 `receive-deploy.sh`/`apply-release.sh`를
   `/opt/innolive/deploy/`에 root:root 755로 복사. `compose.prod.yaml`은
   `/opt/innolive/compose.prod.yaml`로, `innolive-server.service`는
   `/etc/systemd/system/innolive-server.service`로 복사 후 `systemctl daemon-reload`.
   ※ 스크립트 갱신은 항상 "레포 PR 머지 → 서버에 수동 복사" 순서로 한다(자동 동기화 금지 —
   deploy 계정이 스크립트를 덮어쓸 수 있으면 forced command가 무의미해진다).
3. **참조 얼굴 볼륨 권한**: `chown -R 65532:65532 /var/lib/innolive` (컨테이너 non-root uid).
4. **deploy 계정**: `useradd -m -s /bin/bash deploy` 후,
   - `/home/deploy/.ssh/authorized_keys` (600):
     ```
     command="/opt/innolive/deploy/receive-deploy.sh",no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-pty ssh-ed25519 <공개키>
     ```
   - sudoers(`/etc/sudoers.d/innolive-deploy`, 440):
     ```
     deploy ALL=(root) NOPASSWD: /opt/innolive/deploy/apply-release.sh *
     ```
5. **첫 태그 파일**: `printf 'INNOLIVE_TAG=<현재 배포 커밋SHA>\n' > /opt/innolive/deploy/current_tag`
6. 구 preflight 우회(`/etc/innolive/preflight-off.env`)는 유닛 교체와 함께 제거한다.

## GitHub 설정

- Secrets (Actions): `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`(Read&Write),
  `DEPLOY_SSH_KEY`(deploy 계정 개인키), `DEPLOY_SSH_HOST`, `DEPLOY_SSH_PORT`,
  `DEPLOY_SSH_KNOWN_HOSTS`(`ssh-keyscan -p <port> <host>` 고정값).
- 개인 명의 자격증명은 어디에도 사용하지 않는다(팀 서비스 계정만).

## 로그 위생 (public 레포 전제)

- 배포 스크립트 stdout은 러너 로그 = **공개**. 단계 키워드와 커밋 SHA만 출력한다.
- 금지: `set -x`, env 파일 echo/cat, `journalctl`/`docker logs`/`docker inspect`/
  `docker compose config` 출력 중계, `ssh -v`.
- 상세 진단은 서버 로컬 `/opt/innolive/deploy/logs/`(root 600)에만 남긴다.

## 비인증 체험 대기열의 프록시 설정

`GUEST_QUEUE_ENABLED=true`로 비인증 체험 대기열을 운영하고 reverse proxy 또는 ingress를
거치는 경우, `/etc/innolive/server.env`에 proxy가 서버로 연결할 때 사용하는 CIDR만
`GUEST_QUEUE_TRUSTED_PROXY_CIDRS`로 지정한다. 여러 CIDR은 쉼표로 구분한다.

```
GUEST_QUEUE_TRUSTED_PROXY_CIDRS=10.42.0.0/16,fd00:42::/64
```

서버는 직접 연결한 peer가 위 CIDR에 속할 때만 `X-Forwarded-For`를 사용한다. 이때 체인을
오른쪽부터 읽으며 trusted proxy 주소를 건너뛰고, 가장 가까운 untrusted 주소를 rate-limit
키로 삼는다. 따라서 클라이언트가 왼쪽에 임의 주소를 덧붙여도 제한을 우회할 수 없다.

이 값이 비어 있으면 `X-Forwarded-For`를 신뢰하지 않고 TCP peer 주소로 제한한다. proxy
뒤에서 이 값을 비워 두면 proxy 주소 하나를 모든 사용자로 간주하므로, 정상 사용자도 같은
IP별 제한을 공유한다. public CIDR이나 클라이언트 대역을 신뢰 목록에 넣지 않는다.

## 후속 과제

- Docker Hub 계정을 머신 유저 구조로 유지하되 비밀번호 강화 + 2FA 적용.
- 서버에 쌓이는 과거 이미지 정리 정책(디스크 모니터링 후 `docker image prune` 기준 결정).
- `race-test` 잡 안정성 확인 후 필수 검사 승격 검토.
