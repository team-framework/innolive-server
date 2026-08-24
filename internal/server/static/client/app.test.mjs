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
  "resumeBroadcastBtn",
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
  "broadcastYoutubeState",
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
    files: { length: 0 },
    append(...items) {
      this.children.push(...items);
    },
    prepend(item) {
      this.children.unshift(item);
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
    `${source}\nglobalThis.__appTestHooks = { state, els, addRemoteCandidate, flushRemoteCandidateQueue, refreshCurrentSession, runNetworkRecoveryAttempt, startNetworkRecoveryStatusObserver };`,
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
