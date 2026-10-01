import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { validateIssue, validatePullRequest, validateReadiness, validateEvent, validateLinkedIssue } from './collaboration-policy.mjs';

const body = `## 변경 내용

- 카메라 중복 세션 생성 차단

## 검증 결과

- 회귀 테스트 18개 통과
- 실기기 검증 대기

## 머지 체크리스트

- [x] 변경 범위 확인
- [x] 회귀 테스트 완료
- [ ] 실기기 반복 진입 확인
- [x] 리뷰 반영 및 미해결 의견 확인

## 관련 이슈

- Closes #123
`;
const pr = { number: 200, title: 'fix: camera-session/#123', body, head: { ref: 'fix/camera-session/#123' }, user: { login: 'developer' }, draft: true };
const issue = { number: 123, title: 'fix: 카메라 중복 세션 수정', body: `## 작업 내용

- 카메라 세션 충돌 수정

## 작업 체크리스트

- [x] 충돌 조건 확인
- [ ] 수정 검증

## 완료 기준

- 반복 진입 시 카메라 정상 동작
` };

test('Draft PR은 미검증 항목을 보존한 채 형식 검사 통과', () => {
  assert.deepEqual(validatePullRequest(pr).errors, []);
  assert.deepEqual(validateReadiness(pr).errors, []);
});
test('Ready PR의 미완료 필수 항목은 머지 차단', () => {
  assert.deepEqual(validatePullRequest({ ...pr, draft: false }).errors, []);
  assert.match(validateReadiness({ ...pr, draft: false }).errors.join('\n'), /실기기 반복 진입 확인/u);
});
test('체크 후 같은 PR의 머지 검사 통과', () => {
  assert.deepEqual(validateReadiness({ ...pr, draft: false, body: body.replace('- [ ]', '- [x]') }).errors, []);
});
test('필수 공통 확인 항목 삭제로 머지 검사를 우회하지 못함', () => {
  assert.ok(validatePullRequest({ ...pr, body: body.replace('- [x] 리뷰 반영 및 미해결 의견 확인\n', '') }).errors.length);
  assert.ok(validatePullRequest({ ...pr, body: body.replace('- [x] 회귀 테스트 완료\n- [ ] 실기기 반복 진입 확인\n', '') }).errors.length);
});
test('미완료 항목이 있는 Issue는 진행 추적 허용', () => assert.deepEqual(validateIssue(issue).errors, []));
test('GitHub 웹 Issue Form의 H3 섹션도 같은 양식으로 검사', () => assert.deepEqual(validateIssue({ ...issue, body: issue.body.replaceAll('## ', '### ') }).errors, []));
test('Issue의 완료 기준 누락 거부', () => assert.ok(validateIssue({ ...issue, body: issue.body.split('## 완료 기준')[0] }).errors.length));
test('본문 서술형 문단 거부', () => assert.ok(validatePullRequest({ ...pr, body: body.replace('- 카메라 중복 세션 생성 차단', '카메라 중복 세션 생성 차단') }).errors.length));
test('명사형 종결과 다른 말투 거부', () => assert.ok(validatePullRequest({ ...pr, body: body.replace('생성 차단', '생성을 차단했습니다.') }).errors.length));
test('코드 안의 종결 표현은 문체 검사에서 제외', () => assert.deepEqual(validatePullRequest({ ...pr, body: body.replace('생성 차단', '생성 차단: `했습니다`') }).errors, []));
test('구현 과정 섹션 추가 거부', () => assert.ok(validatePullRequest({ ...pr, body: body + '\n## 구현 과정\n\n- 함수 추가\n' }).errors.length));
test('변경 항목 4개 이상 거부', () => assert.ok(validatePullRequest({ ...pr, body: body.replace('- 카메라 중복 세션 생성 차단', '- 카메라 세션 정리\n- 카메라 권한 확인\n- 카메라 입력 교체\n- 카메라 오류 안내') }).errors.length));
test('검증 결과 항목 4개 이상 거부', () => assert.ok(validatePullRequest({ ...pr, body: body.replace('- 실기기 검증 대기', '- Debug 빌드 통과\n- Release 빌드 통과\n- 실기기 검증 대기') }).errors.length));
test('문자열 줄바꿈·빈 체크리스트·템플릿 문구 거부', () => {
  assert.ok(validatePullRequest({ ...pr, body: body.replaceAll('\n', '\\n') }).errors.length);
  assert.ok(validatePullRequest({ ...pr, body: body.replace('- [x] 회귀 테스트 완료\n- [ ] 실기기 반복 진입 확인', '') }).errors.length);
  assert.ok(validatePullRequest({ ...pr, body: body.replace('카메라 중복 세션 생성 차단', '핵심 변경 사항') }).errors.length);
});
test('중복·순서 변경 섹션 거부', () => {
  assert.ok(validatePullRequest({ ...pr, body: body + '\n## 변경 내용\n\n- 중복\n' }).errors.length);
  assert.ok(validatePullRequest({ ...pr, body: body.replace('## 변경 내용', '## 검증 결과').replace('## 검증 결과\n\n- 회귀', '## 변경 내용\n\n- 회귀') }).errors.length);
});
test('제목·브랜치·연결 이슈 번호 불일치 거부', () => {
  assert.ok(validatePullRequest({ ...pr, title: 'fix: 다른 제목' }).errors.length);
  assert.ok(validatePullRequest({ ...pr, body: body.replace('#123', '#124') }).errors.length);
});
test('부분 완료 PR의 Refs 허용', () => assert.deepEqual(validatePullRequest({ ...pr, body: body.replace('Closes', 'Refs') }).errors, []));
test('다른 저장소 이슈만으로 로컬 브랜치 이슈를 대체하지 못함', () => assert.ok(validatePullRequest({ ...pr, body: body.replace('Closes #123', 'Refs other/repo#123') }).errors.length));
test('자동 동기화 예외는 지정 Bot과 고정 브랜치에만 허용', () => {
  const sync = { ...pr, title: 'chore: framework-agent-harness-sync', head: { ref: 'harness-sync/framework-agent' }, user: { login: 'framework-harness-sync[bot]' }, body: body.replace('- Closes #123', '- 관련 이슈 없음: 자동 하네스 동기화') };
  assert.deepEqual(validatePullRequest(sync).errors, []);
  assert.ok(validatePullRequest({ ...sync, user: { login: 'developer' } }).errors.length);
});
test('배포 묶음 PR은 연결 이슈를 Refs로 유지', () => {
  const deploy = { ...pr, title: 'chore: batch-deploy-1001-1', head: { ref: 'chore/batch-deploy-1001-1' }, body: body.replace('Closes', 'Refs') };
  assert.deepEqual(validatePullRequest(deploy).errors, []);
  assert.ok(validatePullRequest({ ...deploy, body }).errors.length);
});
test('기존 기록만 소급 적용 제외', () => {
  const legacy = { ...pr, number: 10, body: '기존 본문', draft: false };
  assert.equal(validateReadiness(legacy, { legacyPullRequestMaxNumber: 10 }).legacy, true);
  assert.ok(validatePullRequest({ ...legacy, number: 11 }, { legacyPullRequestMaxNumber: 10 }).errors.length);
  assert.equal(validateIssue({ ...issue, number: 10, body: '' }, { legacyIssueMaxNumber: 10 }).legacy, true);
});
test('GitHub 이벤트 payload와 직접 JSON 검사 결과 일치', () => {
  assert.deepEqual(validateEvent({ pull_request: pr }, 'pr-format'), validatePullRequest(pr));
  assert.deepEqual(validateEvent({ issue }, 'issue'), validateIssue(issue));
});
test('Issue 선택 참고 항목 허용, 빈 참고와 영어 제목 거부', () => {
  assert.deepEqual(validateIssue({ ...issue, body: issue.body + '\n## 참고\n\n- https://github.com/team-framework/innolive-client/issues/123\n' }).errors, []);
  assert.ok(validateIssue({ ...issue, body: issue.body + '\n## 참고\n' }).errors.length);
  assert.ok(validateIssue({ ...issue, title: 'fix: camera error' }).errors.length);
});
test('연결 Issue가 잘못된 양식이면 PR 검사 실패, 수정 후 통과', async () => {
  const options = value => ({ repository: 'team-framework/innolive-client', token: 'test-token', fetchImpl: async url => {
    assert.equal(url, 'https://api.github.com/repos/team-framework/innolive-client/issues/123');
    return { ok: true, json: async () => value };
  } });
  assert.ok((await validateLinkedIssue(pr, {}, options({ ...issue, body: '서술형 본문' }))).errors.length);
  assert.deepEqual((await validateLinkedIssue(pr, {}, options(issue))).errors, []);
});
test('기존 Issue는 소급 양식 검사 제외, PR 번호를 Issue 대신 연결하면 거부', async () => {
  const options = value => ({ repository: 'team-framework/innolive-client', token: 'test-token', fetchImpl: async () => ({ ok: true, json: async () => value }) });
  assert.deepEqual((await validateLinkedIssue(pr, { legacyIssueMaxNumber: 123 }, options({ ...issue, body: '기존 본문' }))).errors, []);
  assert.ok((await validateLinkedIssue(pr, {}, options({ ...issue, pull_request: {} }))).errors.length);
});
test('연결 Issue 조회 실패는 검사 통과로 취급하지 않음', async () => {
  const result = await validateLinkedIssue(pr, {}, { repository: 'team-framework/innolive-client', token: 'test-token', fetchImpl: async () => ({ ok: false, status: 404 }) });
  assert.ok(result.errors.length);
});
test('검사 CLI의 종료 코드와 Actions 요약으로 실패·복구 확인', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'collaboration-policy-'));
  const event = join(dir, 'event.json');
  const config = join(dir, 'config.json');
  const summary = join(dir, 'summary.md');
  await writeFile(config, '{}');
  await writeFile(event, JSON.stringify({ pull_request: { ...pr, draft: false } }));
  const run = () => spawnSync(process.execPath, [new URL('./collaboration-policy.mjs', import.meta.url).pathname, 'pr-readiness', event, config], { env: { ...process.env, GITHUB_STEP_SUMMARY: summary }, encoding: 'utf8' });
  assert.equal(run().status, 1);
  assert.match(await readFile(summary, 'utf8'), /머지 전 완료 필요/u);
  await writeFile(event, JSON.stringify({ pull_request: { ...pr, draft: false, body: body.replace('- [ ]', '- [x]') } }));
  assert.equal(run().status, 0);
});
