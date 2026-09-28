import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import test from "node:test";
import vm from "node:vm";

const appPath = fileURLToPath(new URL("./app.js", import.meta.url));
const buttonKeys = [
  "signInBtn",
  "signUpBtn",
  "verifyBtn",
  "signOutBtn",
  "startBtn",
  "goLiveBtn",
  "pauseBroadcastBtn",
  "changeResolutionBtn",
  "resumeBroadcastBtn",
  "stopBroadcastBtn",
  "healthBtn",
  "createSessionBtn",
  "refreshSessionsBtn",
  "disconnectBtn",
  "deleteSessionBtn",
  "saveBroadcastBtn",
  "errorProbeBtn",
  "copyJsonBtn",
  "referenceFaceInput",
  "uploadReferenceFaceBtn",
  "refreshReferenceFaceBtn",
  "deleteReferenceFaceBtn",
  "connectChzzkBtn",
  "disconnectChzzkBtn",
  "chzzkCallbackRow",
  "chzzkCallbackUrl",
  "chzzkCompleteRow",
  "completeChzzkBtn",
  "chzzkDetail",
  "broadcastResolution",
  "chzzkCategoryType",
  "chzzkTags",
  "chzzkCategoryQuery",
  "chzzkCategorySearchBtn",
  "chzzkCategoryResults",
  "broadcastSettingsDetail",
  "targetList",
  "targetListEmpty",
  "planSummary",
  "planBadge",
  "planUsage",
  "broadcastModeHint",
  "platformYoutube",
  "platformChzzk",
  "youtubeCard",
  "chzzkCard",
  "youtubeSettings",
  "chzzkSettings",
  "youtubeTitle",
  "youtubePrivacy",
  "youtubeCategory",
  "youtubeThumbnail",
  "youtubeDescription",
  "youtubeMadeForKids",
  "chzzkTitle",
  "chzzkCategoryId",
  "broadcastSettingsState",
  "broadcastAccounts",
  "broadcastPlatformState",
];
const sessionDetailKeys = [
  "sessionJson",
  "sessionId",
  "sessionStatus",
  "aiFallback",
  "videoSenderActive",
  "ignoredTracks",
  "sessionToOffer",
  "offerToAnswer",
  "offerToConnected",
  "answerToConnected",
  "offerToIceDone",
  "answerToIceDone",
  "broadcastRtmpState",
  "localTrackState",
  "remoteTrackState",
  "localVideo",
  "remoteVideo",
  "websocketState",
  "serverUrl",
];

class FakeMediaStream {
  constructor() {
    this.tracks = [];
  }

  getTracks() {
    return this.tracks;
  }

  removeTrack(track) {
    this.tracks = this.tracks.filter((item) => item !== track);
  }
}

function createElement() {
  return {
    children: [],
    dataset: {},
    style: {},
    files: { length: 0 },
    listeners: {},
    addEventListener(type, handler) {
      (this.listeners[type] ||= []).push(handler);
    },
    append(...items) {
      this.children.push(...items);
    },
    prepend(item) {
      this.children.unshift(item);
    },
    replaceChildren(...items) {
      this.children = items;
    },
    get lastElementChild() {
      return this.children.at(-1) || null;
    },
  };
}

async function loadApp({ fetchImpl } = {}) {
  const scheduledTimers = [];
  let fetchCalls = 0;
  const context = {
    Headers,
    FormData,
    URL,
    URLSearchParams,
    console,
    crypto: { randomUUID: () => "00000000-0000-4000-8000-000000000001" },
    async fetch(...args) {
      fetchCalls += 1;
      if (fetchImpl) {
        return fetchImpl(...args);
      }
      throw new Error("fetch must not be called by this test");
    },
    document: {
      addEventListener() {},
      createElement,
    },
    location: { origin: "https://example.test", protocol: "https:" },
    MediaStream: FakeMediaStream,
    WebSocket: { OPEN: 1 },
    window: {
      clearInterval() {},
      clearTimeout() {},
      setInterval() {
        return 1;
      },
      setTimeout(callback, delay) {
        scheduledTimers.push({ callback, delay });
        return scheduledTimers.length;
      },
    },
  };
  context.globalThis = context;

  const source = await readFile(appPath, "utf8");
  vm.runInNewContext(
    `${source}\nglobalThis.__appTestHooks = { state, els, renderTargets, controlTarget, applyPlatformSelection, selectedPlatforms, prepareBroadcast, refreshPlan, modeAvailability, loadYoutubeCategories, setYoutubeCategory, renderSwitchStatus, pauseBroadcast, broadcastControlTargets, addRemoteCandidate, flushRemoteCandidateQueue, queueOrSendCandidate, rememberLocalCandidateGeneration, refreshCurrentSession, runNetworkRecoveryAttempt, startNetworkRecoveryStatusObserver, stopBroadcast, changeBroadcastMode, updateButtons, completeChzzkConnect, saveBroadcastSettings, searchChzzkCategories, applyChzzkCategorySelection, createSession, buildVideoConstraints };`,
    context,
    { filename: appPath },
  );

  const hooks = context.__appTestHooks;
  hooks.els.eventLog = createElement();
  hooks.els.peerState = createElement();
  hooks.els.connectionState = createElement();
  hooks.els.iceState = createElement();
  hooks.els.signalingState = createElement();
  for (const key of buttonKeys) {
    hooks.els[key] = createElement();
  }
  for (const key of sessionDetailKeys) {
    hooks.els[key] = createElement();
  }
  hooks.els.serverUrl.value = "https://example.test";
  return {
    ...hooks,
    scheduledTimers,
    fetchCalls: () => fetchCalls,
  };
}

test("새 세대 후보는 새 answer를 적용할 때까지 queue한다", async () => {
  const { addRemoteCandidate, flushRemoteCandidateQueue, state } = await loadApp();
  const added = [];
  state.pc = {
    remoteDescription: { type: "answer", sdp: "old-answer" },
    async addIceCandidate(candidate) {
      added.push(candidate);
    },
  };
  state.activeNegotiationId = "new-generation";
  state.remoteDescriptionNegotiationId = "old-generation";

  await addRemoteCandidate({
    type: "ice_candidate",
    session_id: "session-1",
    negotiation_id: "new-generation",
    candidate: "candidate:1 1 udp 1 192.0.2.1 5000 typ relay",
    sdpMid: "0",
    sdpMLineIndex: 0,
  });

  assert.equal(added.length, 0);
  assert.equal(state.remoteCandidateQueue.length, 1);

  state.remoteDescriptionNegotiationId = "new-generation";
  await flushRemoteCandidateQueue("new-generation");

  assert.equal(added.length, 1);
  assert.equal(added[0].candidate, "candidate:1 1 udp 1 192.0.2.1 5000 typ relay");
  assert.equal(state.remoteCandidateQueue.length, 0);
});

test("local ICE candidate는 username fragment가 연결한 현재 offer 세대로만 전송한다", async () => {
  const { queueOrSendCandidate, rememberLocalCandidateGeneration, state } = await loadApp();
  const sent = [];
  state.session = { session_id: "session-1" };
  state.ownerToken = "owner-token";
  state.accessToken = "access-token";
  state.ws = {
    readyState: 1,
    send(payload) {
      sent.push(JSON.parse(payload));
    },
  };
  state.offerSent = true;
  state.activeNegotiationId = "new-generation";
  rememberLocalCandidateGeneration(
    { sdp: "v=0\r\na=ice-ufrag:new-ufrag\r\n" },
    "new-generation",
  );

  queueOrSendCandidate({
    candidate: "candidate:1 1 udp 1 192.0.2.1 5000 typ relay ufrag new-ufrag",
    sdpMid: "0",
    sdpMLineIndex: 0,
  });
  queueOrSendCandidate({
    candidate: "candidate:2 1 udp 1 192.0.2.2 5001 typ relay",
    usernameFragment: "old-ufrag",
    sdpMid: "0",
    sdpMLineIndex: 0,
  });
  queueOrSendCandidate(null);

  assert.equal(sent.length, 1);
  assert.equal(sent[0].negotiation_id, "new-generation");
  assert.match(sent[0].candidate, /ufrag new-ufrag$/);
});

test("5초 recovery observer는 stable 상태여도 ICE restart offer를 시작하지 않는다", async () => {
  const { scheduledTimers, startNetworkRecoveryStatusObserver, state } = await loadApp();
  const peerConnection = {
    connectionState: "disconnected",
    iceConnectionState: "checking",
    iceGatheringState: "gathering",
    signalingState: "stable",
  };
  state.pc = peerConnection;
  state.session = { session_id: "session-1" };
  state.recovery.active = true;
  state.recovery.generation = 1;
  state.recovery.attempts = 0;
  state.recovery.deadlineAt = Date.now() + 50_000;

  startNetworkRecoveryStatusObserver(peerConnection, 1, "test");
  assert.equal(scheduledTimers.length, 1);
  assert.equal(scheduledTimers[0].delay, 5000);

  scheduledTimers[0].callback();

  assert.equal(state.recovery.attempts, 0);
  assert.equal(scheduledTimers.length, 2);
  assert.equal(scheduledTimers[1].delay, 5000);
});

test("network recovery는 최초 연결 때 저장한 ICE 설정을 재사용한다", async () => {
  const { els, fetchCalls, runNetworkRecoveryAttempt, state } = await loadApp();
  let restartCalls = 0;
  const peerConnection = {
    connectionState: "disconnected",
    iceConnectionState: "checking",
    iceGatheringState: "gathering",
    signalingState: "stable",
    localDescription: null,
    remoteDescription: null,
    restartIce() {
      restartCalls += 1;
    },
    async createOffer() {
      return { type: "offer", sdp: "offer" };
    },
    async setLocalDescription(description) {
      this.localDescription = description;
      this.signalingState = description.type === "rollback" ? "stable" : "have-local-offer";
    },
    async setRemoteDescription(description) {
      this.remoteDescription = description;
      this.signalingState = "stable";
    },
    async addIceCandidate() {},
  };
  state.pc = peerConnection;
  state.ws = { readyState: 1, send() {} };
  state.session = { session_id: "session-1" };
  state.recovery.active = true;
  state.recovery.generation = 1;
  state.recovery.deadlineAt = Date.now() + 50_000;

  const attempt = runNetworkRecoveryAttempt(peerConnection, 1, "test");
  for (let index = 0; index < 20 && !state.answerWaiter; index += 1) {
    await Promise.resolve();
  }

  assert.equal(fetchCalls(), 0);
  assert.equal(restartCalls, 1);
  assert.ok(
    state.answerWaiter,
    els.eventLog.children.map((item) => item.children[1]?.textContent).join(", "),
  );
  state.answerWaiter.resolve({
    session_id: "session-1",
    negotiation_id: state.activeNegotiationId,
    sdp: "answer",
  });
  await attempt;
  assert.equal(fetchCalls(), 0);
});

test("현재 세션 조회 404는 브라우저 미디어 자원을 함께 정리한다", async () => {
  const { refreshCurrentSession, state } = await loadApp({
    async fetchImpl() {
      return {
        ok: false,
        status: 404,
        statusText: "Not Found",
        async text() {
          return '{"error":{"code":"session_not_found"}}';
        },
      };
    },
  });
  let peerConnectionCloseCalls = 0;
  let webSocketCloseCalls = 0;
  let stoppedTracks = 0;
  const tracks = [
    { kind: "video", stop() { stoppedTracks += 1; } },
    { kind: "audio", stop() { stoppedTracks += 1; } },
  ];
  state.session = { session_id: "expired-session" };
  state.ownerToken = "owner-token";
  state.pc = { close() { peerConnectionCloseCalls += 1; } };
  state.ws = { close() { webSocketCloseCalls += 1; } };
  state.localStream = { getTracks: () => tracks };
  state.pollTimer = 1;

  const result = await refreshCurrentSession();

  assert.equal(result, null);
  assert.equal(peerConnectionCloseCalls, 1);
  assert.equal(webSocketCloseCalls, 1);
  assert.equal(stoppedTracks, 2);
  assert.equal(state.pc, null);
  assert.equal(state.ws, null);
  assert.equal(state.localStream, null);
  assert.equal(state.session, null);
  assert.equal(state.ownerToken, null);
  assert.equal(state.pollTimer, null);
});

test("늦게 도착한 이전 세션의 404는 새 세션 연결을 정리하지 않는다", async () => {
  let resolveFetch;
  const response = new Promise((resolve) => {
    resolveFetch = resolve;
  });
  const { refreshCurrentSession, state } = await loadApp({
    async fetchImpl() {
      return response;
    },
  });
  let peerConnectionCloseCalls = 0;
  let webSocketCloseCalls = 0;
  let stoppedTracks = 0;
  state.session = { session_id: "old-session" };

  const refresh = refreshCurrentSession();
  state.session = { session_id: "new-session" };
  state.ownerToken = "new-owner-token";
  state.pc = { close() { peerConnectionCloseCalls += 1; } };
  state.ws = { close() { webSocketCloseCalls += 1; } };
  state.localStream = {
    getTracks: () => [{ kind: "video", stop() { stoppedTracks += 1; } }],
  };
  resolveFetch({
    ok: false,
    status: 404,
    statusText: "Not Found",
    async text() {
      return '{"error":{"code":"session_not_found"}}';
    },
  });

  await refresh;

  assert.equal(state.session.session_id, "new-session");
  assert.equal(state.ownerToken, "new-owner-token");
  assert.equal(peerConnectionCloseCalls, 0);
  assert.equal(webSocketCloseCalls, 0);
  assert.equal(stoppedTracks, 0);
});

test("방송 종료 버튼은 /stream/stop을 호출하고 세션은 유지한다", async () => {
  const calls = [];
  const { stopBroadcast, state } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ url: String(url), method: options?.method });
      return {
        ok: true,
        status: 200,
        headers: new Headers({ "content-type": "application/json" }),
        async text() {
          return JSON.stringify({ status: "stopped", stop_reason: "user_requested" });
        },
      };
    },
  });
  state.session = { session_id: "session-1", stream: { status: "streaming" } };
  state.ownerToken = "owner-token";
  state.accessToken = "access-token";

  await stopBroadcast();

  // 종료 뒤에는 줄어든 이번 달 방송 시간을 다시 읽는다.
  assert.deepEqual(
    calls.map((call) => call.url.replace("https://example.test", "")),
    ["/sessions/session-1/stream/stop", "/users/me/usage"],
  );
  assert.equal(calls[0].method, "POST");
  // 종료는 egress만 끝낸다 — 세션은 남아 다음 방송을 준비할 수 있어야 한다.
  assert.equal(state.session.session_id, "session-1");
});

test("방송 종료 버튼은 송출이 살아 있을 때만 눌린다", async () => {
  const { updateButtons, state, els } = await loadApp();
  state.session = { session_id: "session-1", stream: { status: "streaming" } };
  updateButtons();
  assert.equal(els.stopBroadcastBtn.disabled, false, "송출 중에는 종료할 수 있어야 한다");

  // 일시 중지·재연결 중에도 방송은 끝낼 수 있어야 한다.
  state.session.stream.status = "paused";
  updateButtons();
  assert.equal(els.stopBroadcastBtn.disabled, false);
  state.session.stream.status = "reconnecting";
  updateButtons();
  assert.equal(els.stopBroadcastBtn.disabled, false);

  // 서버가 ErrStreamNotActive로 보는 상태에서는 막는다.
  state.session.stream.status = "stopped";
  updateButtons();
  assert.equal(els.stopBroadcastBtn.disabled, true);
  state.session = null;
  updateButtons();
  assert.equal(els.stopBroadcastBtn.disabled, true);
});

function jsonResponse(body, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers({ "content-type": "application/json" }),
    async text() {
      return JSON.stringify(body);
    },
  };
}

test("치지직 콜백 state가 인가 요청과 다르면 서버를 호출하지 않는다", async () => {
  const { completeChzzkConnect, state, els, fetchCalls } = await loadApp();
  state.accessToken = "access-token";
  state.chzzkState = "expected-state";
  els.chzzkCallbackUrl.value =
    "https://innolive.studio/auth/chzzk/callback?code=abc&state=other-state";

  await completeChzzkConnect();

  assert.equal(fetchCalls(), 0);
  assert.match(els.chzzkDetail.textContent, /state/);
  // 대조 실패는 인가를 소진하지 않는다 — 같은 state로 다시 붙여넣을 수 있어야 한다.
  assert.equal(state.chzzkState, "expected-state");
});

test("치지직 콜백 URL이 아니면(code만 붙여넣기 포함) 서버를 호출하지 않는다", async () => {
  const { completeChzzkConnect, state, els, fetchCalls } = await loadApp();
  state.accessToken = "access-token";
  state.chzzkState = "expected-state";
  els.chzzkCallbackUrl.value = "abc";

  await completeChzzkConnect();

  assert.equal(fetchCalls(), 0);
});

test("치지직 콜백 state가 일치하면 code·state를 connect에 보내고 계정 목록을 갱신한다", async () => {
  const calls = [];
  const { completeChzzkConnect, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ url: String(url), method: options?.method, body: options?.body });
      if (String(url).endsWith("/auth/chzzk/connect")) {
        return jsonResponse({
          connected: true,
          provider: "chzzk",
          channel: { channelId: "ch-1", channelName: "테스트 채널" },
        });
      }
      return jsonResponse([
        { provider: "chzzk", channel_id: "ch-1", channel_title: "테스트 채널", reconnect_required: false },
      ]);
    },
  });
  state.accessToken = "access-token";
  state.chzzkState = "expected-state";
  els.chzzkCallbackUrl.value =
    "https://innolive.studio/auth/chzzk/callback?code=abc&state=expected-state";

  await completeChzzkConnect();

  assert.equal(calls.length, 2);
  assert.match(calls[0].url, /\/auth\/chzzk\/connect$/);
  assert.equal(calls[0].method, "POST");
  assert.deepEqual(JSON.parse(calls[0].body), { code: "abc", state: "expected-state" });
  assert.match(calls[1].url, /\/auth\/streaming\/accounts$/);
  // 성공하면 state는 일회용으로 폐기된다.
  assert.equal(state.chzzkState, null);
  assert.equal(els.disconnectChzzkBtn.hidden, false);
  assert.match(els.chzzkDetail.textContent, /테스트 채널/);
});

test("치지직 connect 성공 후 계정 목록 조회가 실패해도 연결 실패로 표시하지 않는다", async () => {
  const { completeChzzkConnect, state, els } = await loadApp({
    fetchImpl: async (url) => {
      if (String(url).endsWith("/auth/chzzk/connect")) {
        return jsonResponse({ connected: true, provider: "chzzk", channel: { channelName: "테스트 채널" } });
      }
      throw new Error("network down");
    },
  });
  state.accessToken = "access-token";
  state.chzzkState = "expected-state";
  els.chzzkCallbackUrl.value =
    "https://innolive.studio/auth/chzzk/callback?code=abc&state=expected-state";

  await completeChzzkConnect();

  assert.match(els.chzzkDetail.textContent, /치지직 연결됨: 테스트 채널/);
  assert.equal(state.chzzkState, null);
});

test("치지직 카드는 방송 설정을 category_type·tags로 그 대상 앞으로 저장한다", async () => {
  const calls = [];
  const { saveBroadcastSettings, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ url: String(url), body: options?.body });
      return jsonResponse({ session_id: "s-1", provider: "chzzk", chzzk_broadcast: { title: "제목", category_type: "GAME", category_id: "LoL", tags: ["게임"] } });
    },
  });
  state.accessToken = "access-token";
  state.session = { session_id: "s-1", provider: "chzzk" };
  els.platformChzzk.checked = true;
  els.chzzkTitle.value = "제목";
  els.chzzkCategoryType.value = "GAME";
  els.chzzkCategoryId.value = "LoL";
  els.chzzkTags.value = "게임, 롤, ";

  await saveBroadcastSettings();

  assert.equal(calls.length, 1);
  assert.match(calls[0].url, /\/broadcast\?provider=chzzk$/);
  const body = JSON.parse(calls[0].body);
  assert.deepEqual(body, { title: "제목", category_type: "GAME", category_id: "LoL", tags: ["게임", "롤"] });
  // 유튜브 전용 키가 새어 나가면 서버가 DisallowUnknownFields로 400을 준다.
  assert.equal(body.made_for_kids, undefined);
});

test("플랫폼 카드는 고른 것만 상세 설정을 펴고, 둘 다 고르면 동시 송출이다", async () => {
  const { applyPlatformSelection, selectedPlatforms, modeAvailability, els } = await loadApp();
  els.broadcastResolution.value = "720p";
  els.platformYoutube.checked = true;
  els.platformChzzk.checked = false;
  applyPlatformSelection();
  assert.equal(els.youtubeSettings.hidden, false);
  assert.equal(els.chzzkSettings.hidden, true);
  assert.deepEqual(Array.from(selectedPlatforms()), ["youtube"]);

  els.platformChzzk.checked = true;
  applyPlatformSelection();
  assert.equal(els.chzzkSettings.hidden, false);
  assert.deepEqual(Array.from(selectedPlatforms()), ["youtube", "chzzk"]);
  assert.match(modeAvailability().text, /720p 동시/);

  // 아무것도 고르지 않으면 방송을 열 수 없다.
  els.platformYoutube.checked = false;
  els.platformChzzk.checked = false;
  applyPlatformSelection();
  assert.equal(modeAvailability().allowed, false);
  assert.equal(els.startBtn.disabled, true);
});

test("플랜이 허용하지 않는 송출 방식은 막고, 허용된 방식은 남은 시간을 보인다", async () => {
  const { refreshPlan, modeAvailability, applyPlatformSelection, state, els } = await loadApp({
    fetchImpl: async () =>
      jsonResponse({
        plan: "beam",
        used_seconds: 3600,
        limit_seconds: 432000,
        remaining_seconds: 428400,
        available_by_mode: [
          { mode: "720p_single", multiplier: 1, seconds: 428400, allowed: true },
          { mode: "fhd_single", multiplier: 2, seconds: 214200, allowed: true },
          { mode: "720p_multi", multiplier: 2, seconds: 214200, allowed: true },
          { mode: "fhd_multi", multiplier: 3, seconds: 142800, allowed: false },
        ],
      }),
  });
  state.accessToken = "access-token";
  els.broadcastResolution.value = "fhd";
  els.platformYoutube.checked = true;
  els.platformChzzk.checked = true;

  await refreshPlan();
  applyPlatformSelection();

  assert.equal(els.planSummary.hidden, false);
  assert.equal(els.planBadge.textContent, "Beam 플랜");
  assert.match(els.planUsage.textContent, /119시간 0분 \/ 120시간 0분/);
  assert.equal(modeAvailability().allowed, false);
  assert.match(els.broadcastModeHint.textContent, /Beam 플랜은 FHD 동시 송출을 쓸 수 없습니다/);
  assert.equal(els.startBtn.disabled, true);

  els.platformChzzk.checked = false;
  applyPlatformSelection();
  assert.equal(modeAvailability().allowed, true);
  assert.match(els.broadcastModeHint.textContent, /FHD 단독 · 2배 차감 · 이 방식으로 59시간 30분 가능/);
});

test("YouTube 카테고리는 드롭다운으로 불러오고 목록 밖 값도 잃지 않는다", async () => {
  const { loadYoutubeCategories, setYoutubeCategory, state, els } = await loadApp({
    fetchImpl: async () => jsonResponse({ categories: [{ id: "20", title: "게임" }, { id: "22", title: "인물/블로그" }] }),
  });
  state.accessToken = "access-token";
  els.youtubeCategory.value = "22";

  await loadYoutubeCategories();

  // 없음 + 2건, 고른 값 유지
  assert.equal(els.youtubeCategory.children.length, 3);
  assert.equal(els.youtubeCategory.children[1].value, "20");
  assert.equal(els.youtubeCategory.value, "22");

  setYoutubeCategory("99");
  assert.equal(els.youtubeCategory.children.length, 4);
  assert.equal(els.youtubeCategory.value, "99");
});

test("치지직 카테고리 검색 결과에서 고르면 종류·식별자가 쌍으로 채워진다", async () => {
  const calls = [];
  const { searchChzzkCategories, applyChzzkCategorySelection, els, state } = await loadApp({
    fetchImpl(url) {
      calls.push(url);
      return jsonResponse({
        categories: [
          {
            category_type: "GAME",
            category_id: "League_of_Legends",
            category_value: "리그 오브 레전드",
            poster_image_url: "https://example.test/lol.png",
          },
          // posterImageUrl이 없는 항목도 목록에서 빠지지 않는다.
          { category_type: "GAME", category_id: "Marimo_League", category_value: "마리모 리그", poster_image_url: "" },
        ],
      });
    },
  });
  state.accessToken = "at";
  els.chzzkCategoryQuery.value = " 리그 ";

  await searchChzzkCategories();

  assert.equal(calls.length, 1);
  assert.ok(calls[0].endsWith(`/auth/chzzk/categories?query=${encodeURIComponent("리그")}&size=50`));
  assert.equal(state.chzzkCategories.length, 2);
  // 안내 항목 + 결과 2건.
  assert.equal(els.chzzkCategoryResults.children.length, 3);

  els.chzzkCategoryResults.value = "1";
  applyChzzkCategorySelection();

  assert.equal(els.chzzkCategoryType.value, "GAME");
  assert.equal(els.chzzkCategoryId.value, "Marimo_League");
  // 고른 값을 기본값 주입이 덮지 않도록 touched로 표시한다.
  assert.equal(state.touchedBroadcastFields.has("chzzk:category_id"), true);
  assert.equal(state.touchedBroadcastFields.has("chzzk:category_type"), true);

  // 안내 항목(value="")으로 되돌리면 아무것도 바뀌지 않아야 한다 — Number("")가
  // 0이라 그대로 색인하면 고르지 않은 첫 결과가 적용된다.
  els.chzzkCategoryResults.value = "";
  applyChzzkCategorySelection();
  assert.equal(els.chzzkCategoryId.value, "Marimo_League");
});

test("검색어가 비면 치지직 카테고리 검색을 호출하지 않는다", async () => {
  const { searchChzzkCategories, els, fetchCalls } = await loadApp();
  els.chzzkCategoryQuery.value = "   ";

  await searchChzzkCategories();

  assert.equal(fetchCalls(), 0);
});

test("방송 준비는 고른 플랫폼마다 카드 설정을 저장하고 준비한다", async () => {
  const calls = [];
  const { prepareBroadcast, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method || "GET", body: options?.body });
      return jsonResponse({ session_id: "s-1", provider: "youtube", broadcast_resolution: "720p", targets: [] });
    },
  });
  state.accessToken = "access-token";
  els.platformYoutube.checked = true;
  els.platformChzzk.checked = true;
  els.broadcastResolution.value = "720p";
  els.youtubeTitle.value = "유튜브 제목";
  els.youtubeMadeForKids.checked = false;
  els.chzzkTitle.value = "치지직 제목";
  els.chzzkCategoryId.value = "";
  els.chzzkTags.value = "";
  state.session = { session_id: "s-1", provider: "youtube", broadcast_resolution: "720p" };

  await prepareBroadcast({ session_id: "s-1", provider: "youtube", broadcast_resolution: "720p" });

  const paths = calls.map((call) => `${call.method} ${call.path.replace("https://example.test", "")}`);
  assert.deepEqual(paths, [
    "PUT /sessions/s-1/broadcast?provider=youtube",
    "PUT /sessions/s-1/broadcast?provider=chzzk",
    "POST /sessions/s-1/stream/prepare",
    "POST /sessions/s-1/stream/prepare",
  ]);
  assert.equal(JSON.parse(calls[1].body).title, "치지직 제목");
  assert.deepEqual(JSON.parse(calls[2].body), { provider: "youtube" });
  assert.deepEqual(JSON.parse(calls[3].body), { provider: "chzzk" });
});

test("방송 전 해상도를 바꿨으면 준비 전에 세션 해상도를 맞춘다", async () => {
  const calls = [];
  const { prepareBroadcast, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method || "GET", body: options?.body });
      return jsonResponse({ session_id: "s-1", provider: "youtube", broadcast_resolution: "fhd", targets: [] });
    },
  });
  state.accessToken = "access-token";
  els.platformYoutube.checked = true;
  els.broadcastResolution.value = "fhd";
  els.youtubeTitle.value = "";
  state.session = { session_id: "s-1", provider: "youtube", broadcast_resolution: "720p" };

  await prepareBroadcast({ session_id: "s-1", provider: "youtube", broadcast_resolution: "720p" });

  assert.match(calls[0].path, /\/sessions\/s-1\/broadcast-resolution$/);
  assert.deepEqual(JSON.parse(calls[0].body), { resolution: "fhd" });
});

test("한 플랫폼 준비가 실패해도 나머지는 준비하고 실패를 알린다", async () => {
  const { prepareBroadcast, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      if (String(url).endsWith("/stream/prepare") && JSON.parse(options.body).provider === "chzzk") {
        return jsonResponse({ error: { code: "streaming_not_connected" } }, 409);
      }
      return jsonResponse({ session_id: "s-1", provider: "youtube", targets: [] });
    },
  });
  state.accessToken = "access-token";
  els.platformYoutube.checked = true;
  els.platformChzzk.checked = true;
  els.broadcastResolution.value = "720p";
  els.youtubeTitle.value = "";
  els.chzzkTitle.value = "";
  els.chzzkCategoryId.value = "";
  els.chzzkTags.value = "";
  state.session = { session_id: "s-1", provider: "youtube", broadcast_resolution: "720p" };

  await prepareBroadcast({ session_id: "s-1", provider: "youtube", broadcast_resolution: "720p" });

  assert.equal(state.session.session_id, "s-1");
  assert.match(els.broadcastSettingsDetail.textContent, /준비 실패 — 치지직: streaming_not_connected/);
});

test("개별 제어는 그 대상에만 건다", async () => {
  const calls = [];
  const { renderTargets, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method });
      return jsonResponse({ status: "paused" });
    },
  });
  state.accessToken = "access-token";
  state.session = { session_id: "s-1", provider: "youtube" };

  renderTargets({
    targets: [
      { provider: "chzzk", stream: { status: "streaming", broadcast_phase: "live" } },
      { provider: "youtube", stream: { status: "streaming", broadcast_phase: "live" } },
    ],
  });

  assert.equal(els.targetList.children.length, 2);
  assert.equal(els.targetListEmpty.hidden, true);
  const [chzzkRow] = els.targetList.children;
  // 행마다 일시중지·재개·종료 버튼이 붙는다.
  const buttons = chzzkRow.children.filter((child) => child.type === "button");
  assert.equal(buttons.length, 3);

  await buttons[0].listeners.click[0]();
  assert.equal(calls.length, 1);
  assert.match(calls[0].path, /\/sessions\/s-1\/stream\/pause\?provider=chzzk$/);
  assert.equal(calls[0].method, "POST");
});

test("동시 송출 중 방송 종료는 대상 전부를 끈다", async () => {
  const calls = [];
  const { stopBroadcast, state } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method || "GET" });
      return jsonResponse({ session_id: "s-1", provider: "youtube", targets: [] });
    },
  });
  state.accessToken = "access-token";
  state.session = {
    session_id: "s-1",
    provider: "youtube",
    targets: [
      { provider: "chzzk", stream: { status: "streaming", broadcast_phase: "live" } },
      { provider: "youtube", stream: { status: "streaming", broadcast_phase: "live" } },
    ],
  };

  await stopBroadcast();

  // 대상마다 한 번씩 + 마지막 세션 갱신 한 번.
  const stops = calls.filter((call) => call.path.includes("/stream/stop"));
  assert.equal(stops.length, 2);
  assert.match(stops[0].path, /\/stream\/stop\?provider=chzzk$/);
  assert.match(stops[1].path, /\/stream\/stop\?provider=youtube$/);
});

test("한 대상의 종료가 실패해도 나머지 대상은 계속 끈다", async () => {
  const calls = [];
  const { stopBroadcast, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      const path = String(url);
      calls.push(path);
      if (path.includes("provider=chzzk")) {
        return jsonResponse({ error: { code: "stream_not_active" } }, 409);
      }
      return jsonResponse({ session_id: "s-1", targets: [] });
    },
  });
  state.accessToken = "access-token";
  state.session = {
    session_id: "s-1",
    provider: "youtube",
    targets: [
      { provider: "chzzk", stream: { status: "streaming", broadcast_phase: "live" } },
      { provider: "youtube", stream: { status: "streaming", broadcast_phase: "live" } },
    ],
  };

  await stopBroadcast();

  assert.equal(calls.filter((path) => path.includes("/stream/stop")).length, 2);
  assert.match(els.broadcastSettingsDetail.textContent, /일부 대상 제어 실패 — chzzk: stream_not_active/);
});

test("준비를 거친 대상이 하나뿐이면 상단 버튼은 종전처럼 provider 없이 부른다", async () => {
  const { broadcastControlTargets, state } = await loadApp();
  state.session = {
    session_id: "s-1",
    provider: "youtube",
    targets: [
      { provider: "chzzk", stream: { status: "idle", broadcast_phase: "idle" } },
      { provider: "youtube", stream: { status: "streaming", broadcast_phase: "live" } },
    ],
  };
  assert.deepEqual(Array.from(broadcastControlTargets()), []);

  state.session.targets[0].stream.broadcast_phase = "prepared";
  assert.deepEqual(Array.from(broadcastControlTargets()), ["chzzk", "youtube"]);
});

test("세션 생성은 고른 송출 해상도를 broadcast_resolution으로 보낸다", async () => {
  const calls = [];
  const { createSession, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ url: String(url), method: options?.method, body: options?.body });
      return jsonResponse({ session_id: "s-1", provider: "youtube", broadcast_resolution: "fhd", owner_token: "owner" });
    },
  });
  for (const id of ["sessionLabel", "resolutionSelect", "cameraSelect", "sendAudio"]) {
    els[id] = { value: "", checked: false, dataset: {} };
  }
  state.accessToken = "access-token";
  els.platformYoutube.checked = false;
  els.platformChzzk.checked = true;
  els.broadcastResolution.value = "fhd";

  await createSession();

  const create = calls.find((call) => call.method === "POST" && call.url.endsWith("/sessions"));
  assert.ok(create, "POST /sessions must be sent");
  // 첫 번째로 고른 플랫폼이 세션의 기본 대상이다.
  assert.deepEqual(
    { provider: JSON.parse(create.body).provider, resolution: JSON.parse(create.body).broadcast_resolution },
    { provider: "chzzk", resolution: "fhd" },
  );
});

test("FHD 캡처는 1920x1080을 요청한다", async () => {
  const { buildVideoConstraints, els } = await loadApp();
  els.cameraSelect = { value: "" };
  els.resolutionSelect = { value: "fhd" };
  const video = buildVideoConstraints();
  assert.equal(video.width.ideal, 1920);
  assert.equal(video.height.ideal, 1080);
});

test("방송 중 송출 방식 변경은 해상도와 대상 구성을 broadcast-mode로 보낸다", async () => {
  const calls = [];
  const { changeBroadcastMode, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      const path = String(url);
      calls.push({ path, method: options?.method, body: options?.body });
      if (path.includes("/broadcast/defaults")) {
        return jsonResponse({ title: "직전 제목", category_type: "GAME", category_id: "LoL", tags: [] });
      }
      return jsonResponse({ session_id: "s-1", broadcast_resolution: "720p", targets: [], resolution_switch: { status: "switching" } });
    },
  });
  state.accessToken = "access-token";
  state.session = {
    session_id: "s-1",
    provider: "youtube",
    broadcast_resolution: "fhd",
    targets: [{ provider: "youtube", stream: { status: "streaming", broadcast_phase: "live" } }],
  };
  els.broadcastResolution.value = "720p";
  els.platformYoutube.checked = true;
  els.platformChzzk.checked = true;
  els.chzzkTitle.value = "치지직 제목";
  els.chzzkCategoryId.value = "";
  els.chzzkTags.value = "";

  await changeBroadcastMode();

  const modeCall = calls.find((call) => call.path.endsWith("/sessions/s-1/broadcast-mode"));
  assert.ok(modeCall, `broadcast-mode not called: ${calls.map((call) => call.path).join(", ")}`);
  assert.equal(modeCall.method, "PUT");
  assert.deepEqual(JSON.parse(modeCall.body), { resolution: "720p", targets: ["youtube", "chzzk"] });
  // 새로 추가되는 치지직은 카드 설정을 먼저 저장하고, 이미 라이브인 유튜브는 건드리지 않는다.
  const chzzkSave = calls.find((call) => call.path.includes("/broadcast?provider=chzzk"));
  assert.ok(chzzkSave && calls.indexOf(chzzkSave) < calls.indexOf(modeCall));
  assert.equal(JSON.parse(chzzkSave.body).title, "치지직 제목");
  assert.equal(calls.some((call) => call.path.includes("/broadcast?provider=youtube")), false);
  assert.equal(state.session.resolution_switch.status, "switching");
});

test("송출 방식 변경 버튼은 라이브 중에만 열린다", async () => {
  const { updateButtons, state, els } = await loadApp();
  state.accessToken = "access-token";
  els.platformYoutube.checked = true;
  els.broadcastResolution.value = "720p";
  state.session = { session_id: "s-1", targets: [{ provider: "youtube", stream: { status: "streaming", broadcast_phase: "prepared" } }] };
  updateButtons();
  assert.equal(els.changeResolutionBtn.disabled, true);

  state.session.targets[0].stream.broadcast_phase = "live";
  updateButtons();
  assert.equal(els.changeResolutionBtn.disabled, false);

  // 전환 중에는 다시 누를 수 없다.
  state.session.resolution_switch = { status: "switching" };
  updateButtons();
  assert.equal(els.changeResolutionBtn.disabled, true);
});

test("송출 방식 전환 진행은 상태가 바뀔 때만 알린다", async () => {
  const { renderSwitchStatus, els } = await loadApp({ fetchImpl: async () => jsonResponse({ plan: "plasma" }) });
  const switching = { status: "switching", resolution: "fhd", targets: ["youtube", "chzzk"], started_at: "t1" };
  renderSwitchStatus({ resolution_switch: switching });
  assert.match(els.broadcastSettingsDetail.textContent, /YouTube·치지직 FHD로 바꾸는 중/);

  els.broadcastSettingsDetail.textContent = "다른 메시지";
  renderSwitchStatus({ resolution_switch: switching });
  assert.equal(els.broadcastSettingsDetail.textContent, "다른 메시지");

  renderSwitchStatus({ resolution_switch: { ...switching, status: "done", failed_targets: [{ provider: "chzzk", code: "streaming_rate_limited" }] } });
  assert.match(els.broadcastSettingsDetail.textContent, /실패: 치지직: streaming_rate_limited/);
});
