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
  "disconnectYoutubeBtn",
  "chzzkCallbackRow",
  "chzzkCallbackUrl",
  "chzzkCompleteRow",
  "completeChzzkBtn",
  "chzzkDetail",
  "broadcastResolution",
  "youtubeApplyLiveBtn",
  "chzzkApplyLiveBtn",
  "upgradeOffer",
  "upgradeOfferText",
  "upgradeOfferOptions",
  "sessionNotices",
  "confirmUpgradeBtn",
  "declineUpgradeBtn",
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
  "youtubeChannel",
  "youtubeChannelRow",
  "youtubeThumbnail",
  "youtubeDescription",
  "youtubeMadeForKids",
  "chzzkTitle",
  "chzzkCategoryId",
  "broadcastSettingsState",
  "broadcastAccounts",
  "broadcastPlatformState",
  "authEmail",
  "authPassword",
  "authDetail",
  "authState",
  "verifyRow",
  "verifyCode",
  "connectYoutubeBtn",
  "youtubeDetail",
  "broadcastVideoInput",
  "loginView",
  "viewNav",
  "navStreamBtn",
  "navAdminBtn",
  "streamView",
  "adminView",
  "adminSessionCount",
  "adminSessionsBody",
  "adminUserQuery",
  "adminUsersBody",
  "adminDetail",
  "navDevBtn",
  "devView",
  "devAccountState",
  "devSetupState",
  "devGoogleTokenState",
  "devSetupEmail",
  "devEmpty",
  "devResultBox",
  "devResultTitle",
  "devResult",
  "devAccountTree",
  "devName",
  "devEmail",
  "devPassword",
  "devResetEmail",
  "devResetPassword",
  "devResetCode",
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
    attributes: {},
    setAttribute(name, value) {
      this.attributes[name] = String(value);
    },
    removeAttribute(name) {
      delete this.attributes[name];
    },
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
    localStorage: createStorage(),
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
    `${source}\nglobalThis.__appTestHooks = { state, els, renderTargets, controlTarget, applyPlatformSelection, selectedPlatforms, prepareBroadcast, refreshPlan, modeAvailability, loadYoutubeCategories, setYoutubeCategory, renderSwitchStatus, pauseBroadcast, broadcastControlTargets, addRemoteCandidate, flushRemoteCandidateQueue, queueOrSendCandidate, rememberLocalCandidateGeneration, refreshCurrentSession, runNetworkRecoveryAttempt, startNetworkRecoveryStatusObserver, stopBroadcast, changeBroadcastMode, updateButtons, completeChzzkConnect, saveBroadcastSettings, searchChzzkCategories, applyChzzkCategorySelection, createSession, buildVideoConstraints, syncCaptureResolution, renderUpgradeOffer, acceptUpgradeOffer, confirmUpgradeOption, declineUpgradeOffer, applyLiveSettings, closeSessionOnPageHide, disconnectYoutube, disconnectChzzk, refreshStreamingAccounts, prepareTargetWithConfirm, renderSessionNotices, renderBroadcastWarnings, signIn, showView, renderAdminSessions, closeAdminSession, devRequest, handleDevSocialLogin, maskDevTokens, selectDevFeature, renderDevAccountTree, devSignUp, devRefreshToken, devLogout, devWithdraw, devCheckV1Routes, devPasswordResetStart, devPasswordResetVerify };`,
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

function createStorage() {
  const items = new Map();
  return {
    getItem: (key) => (items.has(key) ? items.get(key) : null),
    setItem: (key, value) => items.set(key, String(value)),
    removeItem: (key) => items.delete(key),
  };
}

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

test("세션 생성이 409면 보관한 이전 세션을 지우고 한 번 다시 만든다", async () => {
  const calls = [];
  let creates = 0;
  const { createSession, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      const path = String(url).replace("https://example.test", "");
      calls.push({ path, method: options?.method, owner: new Headers(options?.headers).get("X-Session-Owner-Token") });
      if (options?.method === "POST" && path === "/sessions") {
        creates += 1;
        if (creates === 2) {
          return jsonResponse({ error: { code: "session_already_exists", message: "exists" } }, 409);
        }
        return jsonResponse({ session_id: `s-${creates}`, owner_token: `owner-${creates}` });
      }
      return jsonResponse({});
    },
  });
  for (const id of ["sessionLabel", "resolutionSelect", "cameraSelect", "sendAudio"]) {
    els[id] = { value: "", checked: false, dataset: {} };
  }
  state.accessToken = "access-token";
  els.platformYoutube.checked = true;

  // 첫 세션을 만들고(보관됨) 새로고침한 것처럼 메모리만 잃는다.
  await createSession();
  state.session = null;
  state.ownerToken = null;
  calls.length = 0;

  const session = await createSession();
  assert.equal(session.session_id, "s-3");
  assert.deepEqual(
    calls.slice(0, 3).map((call) => `${call.method} ${call.path} ${call.owner || ""}`.trim()),
    ["POST /sessions", "DELETE /sessions/s-1 owner-1", "POST /sessions"],
  );
});

test("세션 생성이 409인데 보관한 세션이 없으면 그대로 실패한다", async () => {
  let creates = 0;
  const { createSession, state, els } = await loadApp({
    fetchImpl: async () => {
      creates += 1;
      return jsonResponse({ error: { code: "session_already_exists", message: "exists" } }, 409);
    },
  });
  for (const id of ["sessionLabel", "resolutionSelect", "cameraSelect", "sendAudio"]) {
    els[id] = { value: "", checked: false, dataset: {} };
  }
  state.accessToken = "access-token";
  await assert.rejects(createSession(), /exists/);
  assert.equal(creates, 1);
});

test("페이지를 떠나면 keepalive로 세션 삭제를 보낸다", async () => {
  const calls = [];
  const { closeSessionOnPageHide, state } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), options });
      return jsonResponse({});
    },
  });
  closeSessionOnPageHide();
  assert.equal(calls.length, 0, "no session, nothing to delete");
  state.accessToken = "access-token";
  state.ownerToken = "owner";
  state.session = { session_id: "s-1" };
  closeSessionOnPageHide();
  assert.equal(calls.length, 1);
  assert.match(calls[0].path, /\/sessions\/s-1$/);
  assert.equal(calls[0].options.method, "DELETE");
  assert.equal(calls[0].options.keepalive, true);
  assert.equal(calls[0].options.headers["X-Session-Owner-Token"], "owner");
});

test("유튜브 연결 해제 버튼은 연결돼 있을 때만 보이고 DELETE를 보낸다", async () => {
  const calls = [];
  let accounts = [{ provider: "youtube", channel_title: "채널" }];
  const { disconnectYoutube, refreshStreamingAccounts, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      const path = String(url).replace("https://example.test", "");
      calls.push(`${options?.method || "GET"} ${path}`);
      if (path === "/auth/streaming/accounts") {
        return jsonResponse(accounts);
      }
      if (options?.method === "DELETE") {
        accounts = [];
      }
      return jsonResponse({ items: [] });
    },
  });
  state.accessToken = "access-token";
  await refreshStreamingAccounts();
  assert.equal(els.disconnectYoutubeBtn.hidden, false);
  await disconnectYoutube();
  assert.ok(calls.includes("DELETE /auth/streaming/accounts/youtube"));
  assert.equal(els.disconnectYoutubeBtn.hidden, true);
});

test("유튜브 채널이 여러 개면 고른 채널로 준비하고 그 채널만 해제한다", async () => {
  const calls = [];
  let accounts = [
    { id: "yt-1", provider: "youtube", channel_title: "본 채널" },
    { id: "yt-2", provider: "youtube", channel_title: "브랜드 채널" },
  ];
  const { prepareTargetWithConfirm, disconnectYoutube, refreshStreamingAccounts, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      const path = new URL(url).pathname;
      calls.push({ method: options?.method || "GET", path, body: options?.body ? JSON.parse(options.body) : undefined });
      if (path === "/auth/streaming/accounts") {
        return jsonResponse(accounts);
      }
      if (options?.method === "DELETE") {
        accounts = accounts.filter((account) => !path.endsWith(account.id));
      }
      return jsonResponse({ session_id: "s-1", items: [] });
    },
  });
  state.accessToken = "access-token";
  await refreshStreamingAccounts();
  assert.equal(els.youtubeChannelRow.hidden, false);
  assert.deepEqual(els.youtubeChannel.children.map((option) => option.textContent), ["본 채널", "브랜드 채널"]);
  assert.equal(els.youtubeChannel.value, "yt-1");

  els.youtubeChannel.value = "yt-2";
  await prepareTargetWithConfirm("s-1", "youtube");
  assert.deepEqual(calls.find((call) => call.path === "/sessions/s-1/stream/prepare").body, { provider: "youtube", account_id: "yt-2" });
  // 치지직 준비에는 유튜브 채널을 싣지 않는다.
  await prepareTargetWithConfirm("s-1", "chzzk");
  assert.deepEqual(calls.filter((call) => call.path === "/sessions/s-1/stream/prepare").at(-1).body, { provider: "chzzk" });

  await disconnectYoutube();
  assert.ok(calls.some((call) => call.method === "DELETE" && call.path === "/auth/streaming/accounts/youtube/yt-2"));
  assert.deepEqual(els.youtubeChannel.children.map((option) => option.value), ["yt-1"]);
});

test("직전 방송 값 불러오기를 끄면 카드를 채우지 않는다", async () => {
  const calls = [];
  const { createSession, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push(String(url));
      return jsonResponse({ session_id: "s-1", owner_token: "owner" });
    },
  });
  for (const id of ["sessionLabel", "resolutionSelect", "cameraSelect", "sendAudio"]) {
    els[id] = { value: "", checked: false, dataset: {} };
  }
  state.accessToken = "access-token";
  els.platformChzzk.checked = true;
  els.loadBroadcastDefaults = { checked: false };
  els.chzzkCategoryId.value = "";
  await createSession();
  assert.equal(calls.some((url) => url.includes("/broadcast/defaults")), false);
  assert.equal(els.chzzkCategoryId.value, "");
});

test("방송 중인 플랫폼 연결 해제가 409면 방송을 먼저 끝내라고 안내한다", async () => {
  const { disconnectChzzk, state, els } = await loadApp({
    fetchImpl: async () => jsonResponse({ error: { code: "streaming_account_in_use", message: "in use" } }, 409),
  });
  state.accessToken = "access-token";
  els.disconnectChzzkBtn.hidden = false;
  await disconnectChzzk();
  assert.equal(els.disconnectChzzkBtn.hidden, false);
  assert.match(els.chzzkDetail.textContent, /방송을 먼저 종료하세요/);
});

test("채널이 이미 라이브면 확인받고, 동의하면 allow_concurrent로 다시 준비한다", async () => {
  const bodies = [];
  const { prepareTargetWithConfirm, state } = await loadApp({
    fetchImpl: async (url, options) => {
      bodies.push(JSON.parse(options.body));
      if (!JSON.parse(options.body).allow_concurrent) {
        return jsonResponse({ error: { code: "channel_already_live", message: "live" } }, 409);
      }
      return jsonResponse({ session_id: "s-1" });
    },
  });
  state.accessToken = "access-token";
  await assert.rejects(prepareTargetWithConfirm("s-1", "youtube", () => false), /live/);
  assert.equal(bodies.length, 1);
  const asked = [];
  const prepared = await prepareTargetWithConfirm("s-1", "youtube", (message) => {
    asked.push(message);
    return true;
  });
  assert.equal(prepared.session_id, "s-1");
  assert.match(asked[0], /이미 다른 도구로 라이브/);
  assert.deepEqual(bodies.at(-1), { provider: "youtube", allow_concurrent: true });
});

test("세션 알림은 닫을 때까지 남는 배너로 한 번만 보이고, 준비 경고는 한국어 문구다", async () => {
  const { renderSessionNotices, renderBroadcastWarnings, els } = await loadApp();
  const session = { session_id: "s-1", notices: [{ code: "channel_live_elsewhere" }, { code: "monthly_limit_reached" }] };
  renderSessionNotices(session);
  renderSessionNotices(session);
  assert.equal(els.sessionNotices.children.length, 1, "known notices only, once");
  const [text, close] = els.sessionNotices.children[0].children;
  assert.match(text.textContent, /다른 도구로 방송 중/);
  assert.equal(close.textContent, "닫기");
  renderBroadcastWarnings([{ code: "youtube_quota_low", message: "english" }]);
  assert.match(els.broadcastSettingsDetail.textContent, /유튜브 API 사용량/);
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

const fhdSingleOption = {
  mode: "fhd_single",
  resolution: "fhd",
  targets: ["youtube"],
  units_to: 2,
  remaining_seconds_after: 7200,
  restarts_broadcast: true,
  restart_effects: [{ provider: "youtube", same_link: false }],
};

test("화질 올리기 제안은 선택지마다 버튼이고, 재시작 안내에 동의해야 전환을 요청한다", async () => {
  const calls = [];
  const { renderUpgradeOffer, acceptUpgradeOffer, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method, body: options?.body });
      return jsonResponse({ session_id: "s-1", targets: [], upgrade_offer: null });
    },
  });
  state.accessToken = "access-token";
  state.session = {
    session_id: "s-1",
    targets: [
      { provider: "youtube", stream: { broadcast_phase: "live" } },
      { provider: "chzzk", stream: { broadcast_phase: "idle" } },
    ],
  };
  els.resolutionSelect = createElement();
  els.cameraSelect = createElement();
  els.resolutionSelect.value = "hd";
  els.broadcastResolution.value = "720p";
  const applied = [];
  state.localStream = {
    getVideoTracks: () => [{ async applyConstraints(constraints) { applied.push(constraints); } }],
  };
  renderUpgradeOffer({
    upgrade_offer: {
      units_from: 1,
      options: [fhdSingleOption, { mode: "720p_multi", resolution: "720p", targets: ["youtube", "chzzk"], units_to: 2, needs_settings: true }],
    },
  });
  assert.equal(els.upgradeOffer.hidden, false);
  assert.equal(els.upgradeOfferOptions.children.length, 2);
  assert.match(els.upgradeOfferOptions.children[0].textContent, /1배 → 2배, 약 2시간/);

  // 재시작 안내를 거절하면 아무것도 요청하지 않는다.
  const notices = [];
  await acceptUpgradeOffer(fhdSingleOption, (message) => {
    notices.push(message);
    return false;
  });
  assert.equal(calls.length, 0);
  assert.match(notices[0], /방송이 종료되고 새 방송으로 다시 시작됩니다/);
  assert.match(notices[0], /새 방송 링크/);

  await acceptUpgradeOffer(fhdSingleOption, () => true);
  assert.equal(calls.length, 1);
  assert.match(calls[0].path, /\/sessions\/s-1\/broadcast-mode$/);
  assert.equal(calls[0].method, "PUT");
  assert.deepEqual(JSON.parse(calls[0].body), { resolution: "fhd", targets: ["youtube"] });
  assert.equal(els.broadcastResolution.value, "fhd");
  // 수락하면 캡처도 FHD로 맞춘다(#331).
  assert.equal(els.resolutionSelect.value, "fhd");
  assert.equal(applied.at(-1)?.width.ideal, 1920);
  // 응답에 제안이 없으면 숨긴다.
  assert.equal(els.upgradeOffer.hidden, true);
});

test("플랫폼을 더하는 선택지는 설정 카드를 열고, 확인 버튼이 설정 저장 뒤 전환한다", async () => {
  const calls = [];
  const multi = { mode: "720p_multi", resolution: "720p", targets: ["youtube", "chzzk"], units_to: 2, needs_settings: true, restarts_broadcast: false };
  const liveYoutube = [{ provider: "youtube", stream: { broadcast_phase: "live" } }];
  const { acceptUpgradeOffer, confirmUpgradeOption, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method, body: options?.body });
      return jsonResponse({ session_id: "s-1", targets: liveYoutube, upgrade_offer: { units_from: 1, selected: "720p_multi", options: [multi] } });
    },
  });
  state.accessToken = "access-token";
  state.session = { session_id: "s-1", targets: liveYoutube };
  els.resolutionSelect = createElement();
  els.cameraSelect = createElement();
  els.platformYoutube.checked = true;
  els.platformChzzk.checked = false;
  let asked = false;
  await acceptUpgradeOffer(multi, () => {
    asked = true;
    return true;
  });
  // 재시작이 아니면 안내 없이, 전환이 아니라 선택만 알린다.
  assert.equal(asked, false);
  // 선택을 알리고, 새로 켠 치지직 카드만 직전 방송 값으로 채운다(라이브인 유튜브 폼은 그대로).
  assert.equal(calls.length, 2);
  assert.match(calls[0].path, /\/sessions\/s-1\/upgrade-offer\/select$/);
  assert.match(calls[1].path, /\/broadcast\/defaults\?provider=chzzk$/);
  assert.deepEqual(JSON.parse(calls[0].body), { mode: "720p_multi" });
  assert.equal(els.platformChzzk.checked, true);
  assert.equal(els.chzzkSettings.hidden, false);
  // 고른 뒤에는 다른 선택지를 숨기고 확인 버튼만 보인다.
  assert.equal(els.confirmUpgradeBtn.hidden, false);
  assert.equal(els.upgradeOfferOptions.children.length, 0);
  assert.match(els.upgradeOfferText.textContent, /설정 완료하고 전환/);

  els.chzzkTitle.value = "치지직 제목";
  els.chzzkCategoryId.value = "";
  els.chzzkTags.value = "";
  await confirmUpgradeOption();
  const rest = calls.slice(2);
  assert.equal(rest.length, 2);
  assert.match(rest[0].path, /\/sessions\/s-1\/broadcast\?provider=chzzk$/);
  assert.match(rest[1].path, /\/sessions\/s-1\/broadcast-mode$/);
  assert.deepEqual(JSON.parse(rest[1].body), { resolution: "720p", targets: ["youtube", "chzzk"] });
});

test("치지직 재시작 안내는 같은 주소에서 새 방송으로 시작됨을 보인다", async () => {
  const { acceptUpgradeOffer, state } = await loadApp({ fetchImpl: async () => jsonResponse({}) });
  state.session = { session_id: "s-1", targets: [] };
  const notices = [];
  await acceptUpgradeOffer(
    { mode: "fhd_single", resolution: "fhd", targets: ["chzzk"], restarts_broadcast: true, restart_effects: [{ provider: "chzzk", same_link: true, gap_seconds: 15 }] },
    (message) => {
      notices.push(message);
      return false;
    },
  );
  assert.match(notices[0], /같은 주소에서 약 15초 뒤 새 방송으로 시작/);
  assert.match(notices[0], /새로고침 없이/);
});

test("화질 올리기 거절은 upgrade-offer를 DELETE한다", async () => {
  const calls = [];
  const { declineUpgradeOffer, state } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method });
      return jsonResponse({ session_id: "s-1", targets: [], upgrade_offer: null });
    },
  });
  state.accessToken = "access-token";
  state.session = { session_id: "s-1", targets: [] };
  await declineUpgradeOffer();
  assert.equal(calls.length, 1);
  assert.match(calls[0].path, /\/sessions\/s-1\/upgrade-offer$/);
  assert.equal(calls[0].method, "DELETE");
});

test("방송 중 적용은 바꿀 수 있는 항목만 PATCH로 보낸다", async () => {
  const calls = [];
  const { applyLiveSettings, state, els } = await loadApp({
    fetchImpl: async (url, options) => {
      calls.push({ path: String(url), method: options?.method, body: options?.body });
      return jsonResponse({ session_id: "s-1", targets: [] });
    },
  });
  state.accessToken = "access-token";
  state.session = { session_id: "s-1", targets: [] };
  els.youtubeTitle.value = " 새 제목 ";
  els.youtubeDescription.value = "설명";
  els.youtubeCategory.value = "20";
  els.youtubePrivacy.value = "public";
  els.youtubeMadeForKids.checked = true;
  els.youtubeThumbnail.files = { length: 0 };

  await applyLiveSettings("youtube");
  assert.equal(calls.length, 1);
  assert.match(calls[0].path, /\/sessions\/s-1\/broadcast\/live\?provider=youtube$/);
  assert.equal(calls[0].method, "PATCH");
  // 공개 범위·아동용·썸네일은 방송 중에 바꾸지 않는다.
  assert.deepEqual(JSON.parse(calls[0].body), { title: "새 제목", description: "설명", category_id: "20" });
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

test("송출 해상도를 바꾸면 캡처 해상도가 따라가고 열린 카메라에 바로 적용된다", async () => {
  const { syncCaptureResolution, state, els } = await loadApp();
  els.resolutionSelect = createElement();
  els.cameraSelect = createElement();
  els.resolutionSelect.value = "hd";
  els.cameraSelect.value = "camera-1";
  const applied = [];
  const videoTrack = {
    async applyConstraints(constraints) {
      applied.push(constraints);
    },
    getSettings: () => ({ width: 1920, height: 1080 }),
  };
  state.localStream = { getVideoTracks: () => [videoTrack] };

  els.broadcastResolution.value = "fhd";
  await syncCaptureResolution();
  assert.equal(els.resolutionSelect.value, "fhd");
  assert.equal(applied.length, 1);
  assert.equal(applied[0].width.ideal, 1920);
  assert.equal(applied[0].height.ideal, 1080);
  // 카메라 선택은 트랙을 다시 열지 않으므로 제약에 싣지 않는다.
  assert.equal(applied[0].deviceId, undefined);

  els.broadcastResolution.value = "720p";
  await syncCaptureResolution();
  assert.equal(els.resolutionSelect.value, "hd");
  assert.equal(applied.at(-1).width.ideal, 1280);

  // 이미 맞으면 다시 적용하지 않는다.
  await syncCaptureResolution();
  assert.equal(applied.length, 2);
});

test("카메라가 열려 있지 않으면 캡처 선택만 바꾼다", async () => {
  const { syncCaptureResolution, state, els } = await loadApp();
  els.resolutionSelect = createElement();
  els.cameraSelect = createElement();
  els.resolutionSelect.value = "hd";
  state.localStream = null;

  els.broadcastResolution.value = "fhd";
  await syncCaptureResolution();
  assert.equal(els.resolutionSelect.value, "fhd");
});

test("송출 대상 목록은 방송하지 않는(idle) 대상을 그리지 않는다", async () => {
  const { renderTargets, els } = await loadApp();
  renderTargets({
    targets: [
      { provider: "chzzk", stream: { status: "idle", broadcast_phase: "idle" } },
      { provider: "youtube", stream: { status: "streaming", broadcast_phase: "live" } },
    ],
  });
  assert.equal(els.targetList.children.length, 1);
  assert.equal(els.targetList.children[0].children[0].textContent, "YouTube");
  assert.equal(els.targetListEmpty.hidden, true);

  renderTargets({ targets: [{ provider: "chzzk", stream: { status: "idle", broadcast_phase: "idle" } }] });
  assert.equal(els.targetList.children.length, 0);
  assert.equal(els.targetListEmpty.hidden, false);
});

function signInFetch(adminStatus) {
  const paths = [];
  const fetchImpl = async (url, options = {}) => {
    const path = new URL(url).pathname;
    paths.push(`${options.method || "GET"} ${path}`);
    if (path === "/auth/sign-in") {
      return jsonResponse({ access_token: "access", refresh_token: "refresh", expires_in: 900 });
    }
    if (path === "/admin/me") {
      return adminStatus === 200
        ? jsonResponse({ user_id: "u-1" })
        : jsonResponse({ error: { code: "forbidden", message: "Administrator access is required." } }, adminStatus);
    }
    return jsonResponse({});
  };
  return { fetchImpl, paths };
}

test("관리자가 아니면 로그인 화면에서 넘어가지 못한다", async () => {
  const { fetchImpl } = signInFetch(403);
  const { signIn, state, els } = await loadApp({ fetchImpl });
  els.authEmail.value = "user@example.com";
  els.authPassword.value = "pw";

  await signIn();

  assert.equal(state.accessToken, null);
  assert.equal(state.isAdmin, false);
  assert.equal(els.loginView.hidden, false);
  assert.equal(els.viewNav.hidden, true);
  assert.equal(els.streamView.hidden, true);
  assert.equal(els.adminView.hidden, true);
  assert.match(els.authDetail.textContent, /관리자 계정만/);
});

test("관리자는 송출 테스트로 들어가고 상단바로 어드민 화면만 연다", async () => {
  const { fetchImpl, paths } = signInFetch(200);
  const { signIn, showView, state, els } = await loadApp({ fetchImpl });
  els.authEmail.value = "admin@example.com";
  els.authPassword.value = "pw";

  await signIn();

  assert.equal(state.isAdmin, true);
  assert.equal(els.loginView.hidden, true);
  assert.equal(els.viewNav.hidden, false);
  assert.equal(els.streamView.hidden, false);
  assert.equal(els.adminView.hidden, true);
  assert.equal(els.navStreamBtn.attributes["aria-current"], "page");

  els.adminUserQuery.value = "";
  showView("admin");
  assert.equal(els.streamView.hidden, true);
  assert.equal(els.adminView.hidden, false);
  assert.equal(els.navAdminBtn.attributes["aria-current"], "page");
  assert.equal(els.navStreamBtn.attributes["aria-current"], undefined);
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(paths.includes("GET /admin/sessions"));
  assert.ok(paths.includes("GET /admin/users"));
});

test("어드민 세션 강제 종료는 확인을 거절하면 요청하지 않는다", async () => {
  const methods = [];
  const { closeAdminSession, renderAdminSessions, els } = await loadApp({
    fetchImpl: async (url, options = {}) => {
      methods.push(`${options.method || "GET"} ${new URL(url).pathname}`);
      return options.method === "DELETE" ? { ok: true, status: 204, headers: new Headers(), async text() { return ""; } } : jsonResponse({ sessions: [] });
    },
  });
  const item = { session_id: "s-1", email: "user@example.com", guest: false, status: "connected", targets: [] };
  renderAdminSessions([item]);
  assert.equal(els.adminSessionCount.textContent, "1");
  assert.equal(els.adminSessionsBody.children.length, 1);

  await closeAdminSession(item, () => false);
  assert.deepEqual(Array.from(methods), []);

  await closeAdminSession(item, () => true);
  assert.deepEqual(Array.from(methods), ["DELETE /admin/sessions/s-1", "GET /admin/sessions"]);
  assert.match(els.adminDetail.textContent, /종료했습니다/);
  assert.equal(els.adminSessionCount.textContent, "0");
});

test("개발중 탭 요청은 관리자 토큰이 아닌 테스트 계정 토큰을 쓰고 401에도 관리자 로그인을 유지한다", async () => {
  const seen = [];
  const { devRequest, state, els } = await loadApp({
    fetchImpl: async (url, options = {}) => {
      seen.push(options.headers?.Authorization || null);
      return jsonResponse({ error: { code: "unauthorized" } }, 401);
    },
  });
  state.accessToken = "admin-token";
  state.isAdmin = true;

  const missing = await devRequest("GET", "/auth/login-methods", undefined, { auth: true });
  assert.equal(missing, null);
  assert.equal(seen.length, 0, "dev request without a test account must not fall back to the admin token");
  assert.match(els.devResultTitle.textContent, /먼저 로그인/);

  state.dev.accessToken = "dev-token";
  const result = await devRequest("GET", "/auth/login-methods", undefined, { auth: true });
  assert.equal(result.status, 401);
  assert.deepEqual(Array.from(seen), ["Bearer dev-token"]);
  assert.equal(state.accessToken, "admin-token");
  assert.equal(els.devResultTitle.dataset.state, "error");
});

test("v2 소셜 로그인 결과가 테스트 계정 상태에 반영된다", async () => {
  const { handleDevSocialLogin, state, els } = await loadApp();
  els.devSetupEmail.value = "";
  handleDevSocialLogin(
    { ok: false, status: 403, payload: { error: { code: "password_setup_required" }, setup_token: "setup-123" } },
    "구글(legacy@gmail.com)",
    "legacy@gmail.com",
  );
  assert.equal(state.dev.setupToken, "setup-123");
  assert.equal(state.dev.accessToken, null);
  assert.equal(els.devSetupEmail.value, "legacy@gmail.com");
  assert.match(els.devSetupState.textContent, /setup_token 있음/);

  handleDevSocialLogin({ ok: true, status: 200, payload: { access_token: "access-1" } }, "구글(member@gmail.com)", null);
  assert.equal(state.dev.accessToken, "access-1");
  assert.equal(state.dev.setupToken, null);
  assert.match(els.devAccountState.textContent, /member@gmail.com/);
});

test("개발중 결과 표시는 토큰 값을 앞부분만 남긴다", async () => {
  const { maskDevTokens } = await loadApp();
  const masked = maskDevTokens({ access_token: "abcdefghijklmnopqrstuvwxyz", token_type: "Bearer", error: { code: "x" }, setup_token: "short" });
  assert.equal(masked.access_token, "abcdefghijkl…");
  assert.equal(masked.token_type, "Bearer");
  assert.equal(masked.setup_token, "short");
  assert.equal(masked.error.code, "x");
});

test("개발중 기능을 고르면 그 기능 화면만 보인다", async () => {
  const { selectDevFeature, state, els } = await loadApp();
  const panel = (name) => Object.assign(createElement(), { dataset: { devPanel: name }, hidden: true });
  const button = (name) => Object.assign(createElement(), { dataset: { devFeature: name } });
  els.devPanels = [panel("email"), panel("setup")];
  els.devFeatureButtons = [button("email"), button("setup")];

  selectDevFeature("setup");
  assert.equal(state.dev.feature, "setup");
  assert.equal(els.devEmpty.hidden, true);
  assert.equal(els.devPanels[0].hidden, true);
  assert.equal(els.devPanels[1].hidden, false);
  assert.equal(els.devFeatureButtons[1].attributes["aria-current"], "true");
  assert.equal(els.devFeatureButtons[0].attributes["aria-current"], undefined);
});

test("개발중 계정 카드는 InnoLive 계정 아래에 로그인 수단 연결 상태를 그린다", async () => {
  const { renderDevAccountTree, state, els } = await loadApp();
  state.dev.accessToken = "dev-token";
  state.dev.name = "홍길동";
  state.dev.methods = { email: "member@example.com", providers: [{ provider: "google", email: "member@gmail.com" }] };

  renderDevAccountTree();
  const [root, email, google, apple] = els.devAccountTree.children;
  assert.equal(root.textContent, "InnoLive 계정");
  assert.equal(root.children[0].textContent, "홍길동 · member@example.com");
  assert.equal(email.dataset.linked, "true");
  assert.equal(google.textContent, "구글 ✓ member@gmail.com");
  assert.equal(apple.dataset.linked, "false");

  state.dev.accessToken = null;
  state.dev.methods = null;
  state.dev.setupToken = "setup-1";
  renderDevAccountTree();
  assert.equal(els.devAccountTree.children[0].textContent, "기존 소셜 계정 (이메일 계정 없음)");
});

test("개발중 계정 카드는 구글 연결을 하나씩 그리고 연결 ID로 해제한다", async () => {
  const calls = [];
  const { renderDevAccountTree, state, els } = await loadApp({
    fetchImpl: async (url, options = {}) => {
      calls.push(`${options.method || "GET"} ${new URL(url).pathname}`);
      if (options.method === "DELETE") {
        return { ok: true, status: 204, headers: new Headers(), async text() { return ""; } };
      }
      return jsonResponse({ email: "member@example.com", providers: [] });
    },
  });
  state.dev.accessToken = "dev-token";
  state.dev.methods = {
    email: "member@example.com",
    providers: [
      { id: "link-1", provider: "google", email: "first@gmail.com" },
      { id: "link-2", provider: "google", email: "second@gmail.com" },
    ],
  };

  renderDevAccountTree();
  const labels = els.devAccountTree.children.map((item) => item.textContent);
  assert.ok(labels.includes("구글 1 ✓ first@gmail.com"));
  assert.ok(labels.includes("구글 2 ✓ second@gmail.com"));
  assert.ok(labels.includes("애플 · 미연결"));

  const second = els.devAccountTree.children.find((item) => item.textContent.startsWith("구글 2"));
  const [unlink] = second.children;
  assert.equal(unlink.textContent, "해제");
  await unlink.listeners.click[0]();
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(calls.includes("DELETE /auth/link/google/link-2"), calls.join(", "));
});

test("개발중 이메일 가입은 이름을 필수로 v2 가입에 보낸다", async () => {
  const calls = [];
  const { devSignUp, state, els } = await loadApp({
    fetchImpl: async (url, options = {}) => {
      calls.push({ path: new URL(url).pathname, body: JSON.parse(options.body) });
      return jsonResponse({ status: "verification_email_sent", signup_token: "signup-1" });
    },
  });
  els.devName.value = "";
  els.devEmail.value = "member@example.com";
  els.devPassword.value = "correct horse battery staple";
  await devSignUp();
  assert.equal(calls.length, 0);
  assert.match(els.devResultTitle.textContent, /이름/);

  els.devName.value = " 홍길동 ";
  await devSignUp();
  assert.equal(calls[0].path, "/auth/v2/native/sign-up");
  assert.equal(calls[0].body.name, "홍길동");
  assert.equal(state.dev.signupToken, "signup-1");
});

test("개발중 계정 카드에 병합 확인용 플랜과 송출 연결이 함께 보인다", async () => {
  const { renderDevAccountTree, state, els } = await loadApp();
  state.dev.accessToken = "dev-token";
  state.dev.methods = { email: "member@example.com", providers: [] };
  state.dev.usage = { plan: "plasma" };
  state.dev.streamingAccounts = [{ provider: "youtube", channel_title: "내 채널" }];

  renderDevAccountTree();
  const items = els.devAccountTree.children;
  assert.match(items[0].children[0].textContent, /플랜 plasma/);
  const labels = items.map((item) => item.textContent);
  assert.ok(labels.includes("송출 · 유튜브 ✓ 내 채널"));
  assert.ok(labels.includes("송출 · 치지직 · 미연결"));
});

test("개발중 토큰 갱신·로그아웃·탈퇴는 테스트 계정 상태만 바꾼다", async () => {
  const calls = [];
  const { devRefreshToken, devLogout, devWithdraw, state } = await loadApp({
    fetchImpl: async (url, options = {}) => {
      const path = new URL(url).pathname;
      calls.push(`${options.method} ${path}`);
      if (path === "/auth/refresh") {
        return jsonResponse({ access_token: "access-2", refresh_token: "refresh-2" });
      }
      if (options.method === "DELETE" || path === "/auth/logout") {
        return { ok: true, status: 204, headers: new Headers(), async text() { return ""; } };
      }
      return jsonResponse({});
    },
  });
  state.accessToken = "admin-token";
  state.dev.accessToken = "access-1";
  state.dev.refreshToken = "refresh-1";

  await devRefreshToken();
  assert.equal(state.dev.accessToken, "access-2");
  assert.equal(state.dev.refreshToken, "refresh-2");

  await devWithdraw(() => false);
  assert.ok(!calls.includes("DELETE /auth/me"), "declined withdrawal must not send a request");
  await devWithdraw(() => true);
  assert.ok(calls.includes("DELETE /auth/me"));
  assert.equal(state.dev.accessToken, null);
  assert.equal(state.accessToken, "admin-token");

  state.dev.accessToken = "access-3";
  state.dev.refreshToken = "refresh-3";
  await devLogout();
  assert.ok(calls.includes("POST /auth/logout"));
  assert.equal(state.dev.refreshToken, null);
});

test("v1 종료 점검은 400을 남아 있음, 404를 제거됨으로 가른다", async () => {
  const { devCheckV1Routes, els } = await loadApp({
    fetchImpl: async (url) => jsonResponse({}, new URL(url).pathname === "/auth/native/sign-up" ? 400 : 404),
  });
  const rows = await devCheckV1Routes();
  assert.deepEqual(JSON.parse(JSON.stringify(rows.map((row) => row.state))), ["제거됨", "제거됨", "남아 있음"]);
  assert.equal(els.devResultTitle.dataset.state, "error");
});

test("개발중 비밀번호 변경은 코드 확인 후 새 토큰으로 테스트 계정을 로그인시킨다", async () => {
  const calls = [];
  const { devPasswordResetStart, devPasswordResetVerify, state, els } = await loadApp({
    fetchImpl: async (url, options = {}) => {
      const path = new URL(url).pathname;
      calls.push({ path, body: options.body ? JSON.parse(options.body) : null, auth: options.headers?.Authorization || null });
      if (path === "/auth/password/reset") {
        return jsonResponse({ status: "verification_email_sent", reset_token: "reset-1" });
      }
      if (path === "/auth/password/reset/verify") {
        return jsonResponse({ access_token: "access-new", refresh_token: "refresh-new" });
      }
      return jsonResponse({});
    },
  });
  els.devResetCode.value = "";
  await devPasswordResetVerify();
  assert.equal(calls.length, 0, "verify without a reset token must not send a request");

  els.devResetEmail.value = "member@example.com";
  els.devResetPassword.value = "new password 456";
  await devPasswordResetStart();
  assert.equal(calls[0].path, "/auth/password/reset");
  assert.equal(calls[0].body.new_password, "new password 456");
  assert.equal(calls[0].auth, null, "password reset must not need a logged-in token");
  assert.equal(state.dev.resetToken, "reset-1");

  els.devResetCode.value = "123456";
  await devPasswordResetVerify();
  assert.equal(calls[1].path, "/auth/password/reset/verify");
  assert.deepEqual(JSON.parse(JSON.stringify(calls[1].body)), { reset_token: "reset-1", verification_code: "123456" });
  assert.equal(state.dev.accessToken, "access-new");
  assert.equal(state.dev.refreshToken, "refresh-new");
  assert.equal(state.dev.resetToken, null);
});
