import { readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

const ISSUE_HEADINGS = ['작업 내용', '작업 체크리스트', '완료 기준'];
const PR_HEADINGS = ['변경 내용', '검증 결과', '머지 체크리스트', '관련 이슈'];
const ISSUE_TITLE = /^(feat|fix|chore|refactor): \S.*$/u;
const BRANCH = /^(feat|fix|chore|refactor)\/[a-z0-9]+(?:-[a-z0-9]+)*\/#([1-9]\d*)$/u;
const SENTENCE_ENDING = /(?:했습니다|하였습니다|되었습니다|합니다|됩니다|입니다|습니다|했어요|해요|이에요|예요|있어요|없어요|할게요|주세요|줘요|한다|했다|하였다|된다|되었다|됐다|있다|없다|이다|였다)[.!。]*$/u;
const PLACEHOLDER = /^(?:핵심 변경 사항|실행한 검증과 결과|검증 결과 입력|작업 내용 입력|완료 조건 입력|관련 이슈 입력|TODO|TBD|내용 입력|\.{3})$/iu;

function error(errors, message) { errors.push(message); }

function parseBody(body, headings, optionalHeadings = [], allowFormHeadings = false) {
  const errors = [];
  if (typeof body !== 'string' || !body.trim()) return { errors: ['본문 필수'], sections: new Map() };
  if (body.includes('\\n')) error(errors, '문자열 \\n 대신 실제 줄바꿈 사용');
  if (body.includes('—')) error(errors, 'em dash 사용 금지');
  const clean = body.replace(/<!--[\s\S]*?-->/gu, '').replace(/\r\n?/gu, '\n');
  const sections = new Map();
  const found = [];
  let current;
  for (const line of clean.split('\n')) {
    if (!line.trim()) continue;
    const heading = (allowFormHeadings ? /^#{2,3} (.+)$/u : /^## (.+)$/u).exec(line);
    if (heading) {
      current = heading[1].trim();
      if (sections.has(current)) error(errors, `중복 섹션: ${current}`);
      sections.set(current, []);
      found.push(current);
      continue;
    }
    if (!current) { error(errors, '섹션 밖 내용 금지'); continue; }
    sections.get(current).push(line);
  }
  const expected = [...headings, ...optionalHeadings.filter(name => sections.has(name))];
  if (JSON.stringify(found) !== JSON.stringify(expected)) error(errors, `섹션 순서: ${expected.join(' / ')}`);
  for (const name of headings) if (!sections.get(name)?.length) error(errors, `필수 내용: ${name}`);
  return { errors, sections };
}

function bullets(lines = [], name, errors, { max = Infinity, checklist = false, allowLinks = false } = {}) {
  if (lines.length > max) error(errors, `${name}: 최대 ${max}개 항목`);
  const items = [];
  for (const line of lines) {
    const match = checklist ? /^- \[([ xX])\] (\S.*)$/u.exec(line) : /^- (\S.*)$/u.exec(line);
    if (!match || (!checklist && /^- \[[ xX]\] /u.test(line))) {
      error(errors, `${name}: ${checklist ? '- [ ] 또는 - [x]' : '-'} 개조식 항목만 허용`);
      continue;
    }
    const text = checklist ? match[2] : match[1];
    if (PLACEHOLDER.test(text.trim())) error(errors, `${name}: 예시 문구 대신 실제 내용 작성`);
    const prose = text.replace(/`[^`]*`/gu, '').replace(/\[([^\]]+)\]\([^)]+\)/gu, '$1').trim();
    if (SENTENCE_ENDING.test(prose)) error(errors, `${name}: 명사형 종결 사용`);
    if (!allowLinks && /^https?:\/\//u.test(text)) error(errors, `${name}: 링크 대신 핵심 내용 작성`);
    items.push({ text, checked: checklist && match[1].toLowerCase() === 'x' });
  }
  return items;
}

export function validateIssue(issue, { legacyIssueMaxNumber = 0 } = {}) {
  if (issue.number > 0 && issue.number <= legacyIssueMaxNumber) return { errors: [], legacy: true };
  const { errors, sections } = parseBody(issue.body, ISSUE_HEADINGS, ['참고'], true);
  if (!ISSUE_TITLE.test(issue.title ?? '') || !/[가-힣]/u.test(issue.title ?? '')) error(errors, 'Issue 제목: <type>: <한국어 작업 내용>');
  bullets(sections.get('작업 내용'), '작업 내용', errors);
  bullets(sections.get('작업 체크리스트'), '작업 체크리스트', errors, { checklist: true });
  bullets(sections.get('완료 기준'), '완료 기준', errors);
  if (sections.has('참고')) {
    if (!sections.get('참고').length) error(errors, '빈 참고 섹션 생략');
    bullets(sections.get('참고'), '참고', errors, { allowLinks: true });
  }
  return { errors };
}

function pullRequestKind(pr, errors) {
  const branch = pr.head?.ref ?? '';
  const actor = pr.user?.login ?? '';
  if (branch === 'harness-sync/framework-agent' && /^framework-harness-sync(?:\[bot\])?$/u.test(actor)) {
    if (pr.title !== 'chore: framework-agent-harness-sync') error(errors, '자동 동기화 PR 제목 불일치');
    return { kind: 'sync' };
  }
  if (/^chore\/batch-deploy-\d{4}-\d+$/u.test(branch)) {
    if (pr.title !== branch.replace('/', ': ')) error(errors, '배포 묶음 PR 제목과 브랜치 불일치');
    return { kind: 'deploy' };
  }
  const match = BRANCH.exec(branch);
  if (!match) error(errors, '브랜치: <type>/<english-slug>/#<issue-number>');
  if (pr.title !== branch.replace('/', ': ')) error(errors, 'PR 제목과 head 브랜치 불일치');
  return { kind: 'normal', issueNumber: match?.[2] };
}

export function validatePullRequest(pr, { legacyPullRequestMaxNumber = 0 } = {}) {
  if (pr.number > 0 && pr.number <= legacyPullRequestMaxNumber) return { errors: [], legacy: true };
  const { errors, sections } = parseBody(pr.body, PR_HEADINGS);
  const kind = pullRequestKind(pr, errors);
  bullets(sections.get('변경 내용'), '변경 내용', errors, { max: 3 });
  bullets(sections.get('검증 결과'), '검증 결과', errors, { max: 3 });
  const checklist = bullets(sections.get('머지 체크리스트'), '머지 체크리스트', errors, { checklist: true });
  for (const required of ['변경 범위 확인', '리뷰 반영 및 미해결 의견 확인']) {
    if (!checklist.some(item => item.text === required)) error(errors, `필수 체크 항목: ${required}`);
  }
  if (checklist.length < 3) error(errors, '머지 체크리스트: 공통 확인 2개와 변경에 맞는 검증 항목 필요');
  const references = [];
  for (const line of sections.get('관련 이슈') ?? []) {
    const reference = /^(?:- )?(Closes|Fixes|Resolves|Refs) (?:(?:[\w.-]+)\/(?:[\w.-]+))?#([1-9]\d*)$/iu.exec(line);
    if (reference) { references.push(reference); continue; }
    if (kind.kind === 'sync' && line === '- 관련 이슈 없음: 자동 하네스 동기화') continue;
    error(errors, '관련 이슈: Closes #번호 또는 Refs #번호');
  }
  if (kind.kind === 'normal' && kind.issueNumber && !references.some(ref => ref[2] === kind.issueNumber && /^(?:- )?(Closes|Fixes|Resolves|Refs) #/iu.test(ref[0]))) {
    error(errors, '브랜치의 이슈 번호를 관련 이슈에 기재');
  }
  if (kind.kind === 'deploy' && !references.length) error(errors, '배포 묶음의 관련 이슈 기재');
  if (kind.kind === 'deploy' && references.some(ref => ref[1].toLowerCase() !== 'refs')) error(errors, '배포 묶음은 Refs 사용');
  return { errors };
}

export function validateReadiness(pr, config = {}) {
  const result = validatePullRequest(pr, config);
  if (result.legacy || pr.draft) return result;
  const { sections } = parseBody(pr.body, PR_HEADINGS);
  const items = bullets(sections.get('머지 체크리스트'), '머지 체크리스트', [] , { checklist: true });
  for (const item of items) if (!item.checked) error(result.errors, `머지 전 완료 필요: ${item.text}`);
  return result;
}

export async function validateLinkedIssue(pr, config, { repository, token, fetchImpl = fetch }) {
  if (pr.number > 0 && pr.number <= (config.legacyPullRequestMaxNumber ?? 0)) return { errors: [], legacy: true };
  const match = BRANCH.exec(pr.head?.ref ?? '');
  if (!match) return { errors: [] };
  if (!/^[\w.-]+\/[\w.-]+$/u.test(repository ?? '') || !token) return { errors: ['연결 Issue 검사 인증·저장소 정보 필요'] };
  const response = await fetchImpl(`https://api.github.com/repos/${repository}/issues/${match[2]}`, {
    headers: { Accept: 'application/vnd.github+json', Authorization: `Bearer ${token}`, 'X-GitHub-Api-Version': '2022-11-28' }
  });
  if (!response.ok) return { errors: [`연결 Issue #${match[2]} 조회 실패: HTTP ${response.status}`] };
  const issue = await response.json();
  if (issue.pull_request || String(issue.number) !== match[2]) return { errors: ['브랜치 번호는 실제 Issue에 연결 필요'] };
  const result = validateIssue(issue, config);
  return { ...result, errors: result.errors.map(message => `Issue #${match[2]}: ${message}`) };
}

export function validateEvent(payload, mode, config = {}) {
  if (mode === 'issue') return validateIssue(payload.issue ?? payload, config);
  if (mode === 'pr-format') return validatePullRequest(payload.pull_request ?? payload, config);
  if (mode === 'pr-readiness') return validateReadiness(payload.pull_request ?? payload, config);
  throw new Error(`지원하지 않는 검사: ${mode}`);
}

async function main() {
  const [mode, eventPath = process.env.GITHUB_EVENT_PATH, configPath = '.github/collaboration-policy.json'] = process.argv.slice(2);
  if (!eventPath) throw new Error('검사할 JSON 파일 경로 필요');
  let config = {};
  try { config = JSON.parse(await readFile(configPath, 'utf8')); }
  catch (cause) { if (cause.code !== 'ENOENT') throw cause; }
  const payload = JSON.parse(await readFile(eventPath, 'utf8'));
  const result = mode === 'pr-issue'
    ? await validateLinkedIssue(payload.pull_request ?? payload, config, { repository: process.env.GITHUB_REPOSITORY, token: process.env.GH_TOKEN })
    : validateEvent(payload, mode, config);
  const report = result.legacy ? '기존 기록: 새 양식 소급 적용 제외' : result.errors.length ? result.errors.join('\n') : '검사 통과';
  if (process.env.GITHUB_STEP_SUMMARY) await writeFile(process.env.GITHUB_STEP_SUMMARY, `## ${mode}\n\n${report.split('\n').map(line => `- ${line}`).join('\n')}\n`, { flag: 'a' });
  console.log(report);
  process.exitCode = result.errors.length ? 1 : 0;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main();
