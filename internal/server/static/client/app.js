const DEFAULT_SERVER_URL = "https://innolive.duckdns.org";
const SERVER_STORAGE_KEY = "inno-live-client.server-url";
const CLIENT_ID_STORAGE_KEY = "inno-live-client.client-id";
// 마지막으로 만든 세션(id·owner token). 새로고침으로 잃은 세션을 409 때 정리하는 데 쓴다.
const LAST_SESSION_STORAGE_KEY = "inno-live-client.last-session";
const MAX_LOG_ITEMS = 120;
const DEFAULT_ICE_SERVERS = [{ urls: "stun:stun.l.google.com:19302" }];
const VIDEO_TRACK_WAIT_ATTEMPTS = 15;
const VIDEO_TRACK_WAIT_INTERVAL_MS = 1000;
const DEFAULT_RECOVERY_POLICY = {
  window_ms: 50000,
  debounce_ms: 2000,
  max_attempts: 10,
};
// 복구 창 안에서 ICE restart offer는 한 번만 보낸다. 이후에는 브라우저가 이미
// 시작한 STUN/TURN 후보 수집·연결 검사를 유지하고, 이 간격으로 로컬 상태만
// 관찰한다. 이 타이머는 HTTP·WebSocket 요청을 보내지 않는다.
const RECOVERY_STATUS_CHECK_INTERVAL_MS = 5000;
// trickle ICE answer는 후보 수집 완료를 기다리지 않고 도착한다. 다만 느린
// 네트워크에서 signaling 응답 자체가 늦을 수 있으므로, 일반 협상보다 긴 시간을
// 허용하되 연결 검사를 위한 여유를 복구 창에 남긴다.
const RECOVERY_ANSWER_TIMEOUT_MS = 35000;
const RECOVERY_CONNECTION_RESERVE_MS = 5000;

const els = {};
const state = {
  busy: false,
  referenceFaceBusy: false,
  referenceFaceSupported: null,
  referenceFace: null,
  referenceFacePreviewUrl: null,
  session: null,
  ownerToken: null,
  shownNotices: new Set(),
  accessToken: null,
  refreshToken: null,
  authEmail: null,
  // 로그인한 사용자가 ADMIN_USER_IDS에 있는지(#373). 관리자만 로그인 화면을 넘는다.
  isAdmin: false,
  // 상단바에서 고른 화면: "stream"(송출 테스트) 또는 "admin"(어드민).
  view: "stream",
  refreshPromise: null,
  signupToken: null,
  // 치지직 인가 요청에 실은 state. 콜백에서 돌아온 값과 대조한 뒤 폐기한다.
  chzzkState: null,
  pc: null,
  ws: null,
  localStream: null,
  remoteStream: new MediaStream(),
  pollTimer: null,
  answerWaiter: null,
  candidateQueue: [],
  remoteCandidateQueue: [],
  offerSent: false,
  activeNegotiationId: null,
  // local candidate는 candidate.usernameFragment로 offer 세대를 식별한다. 새
  // ICE restart offer를 만들기 시작한 뒤에도 이전 세대 candidate 이벤트가 늦게
  // 도착할 수 있으므로 activeNegotiationId만으로 후보를 표기하면 안 된다.
  localCandidateNegotiationIds: new Map(),
  // remoteDescription이 존재하더라도 현재 offer 세대의 answer가 아닐 수 있다.
  // ICE restart 중 이전 answer에 새 세대 후보를 적용하지 않도록, answer 적용이
  // 완료된 negotiation ID를 별도로 추적한다.
  remoteDescriptionNegotiationId: null,
  webrtcConfig: {
    iceServers: DEFAULT_ICE_SERVERS,
    recovery: DEFAULT_RECOVERY_POLICY,
  },
  closingConnection: false,
  recovery: {
    active: false,
    attempts: 0,
    deadlineAt: 0,
    generation: 0,
    debounceTimer: null,
    statusTimer: null,
    initialOfferPending: false,
    reason: null,
  },
  lastSelectedCandidatePairId: null,
  lastSessionJson: null,
  // 사용자가 직접 건드린 방송 설정 필드. 직전 방송 기본값(#143)이 이 필드를
  // 덮으면 고른 공개 범위·아동용 신고가 뒤집히므로 여기에 담아 보존한다.
  touchedBroadcastFields: new Set(),
  // 치지직 카테고리 검색 결과. select의 값(색인)으로 종류·식별자 쌍을 되찾는다.
  chzzkCategories: [],
  // 방송 설정을 한 번이라도 저장했는지. 미협상 세션이 회수돼 새 세션으로 이어갈
  // 때(#147) 저장했던 설정도 함께 사라지므로 폼 값을 다시 보내야 한다.
  broadcastSettingsSaved: false,
  // 요금제·이번 달 사용량(GET /users/me/usage). 송출 방식별 허용·남은 시간을 쓴다.
  usage: null,
  // 이 세션에서 직전 방송 기본값을 채운 플랫폼. 나중에 고른 플랫폼도 한 번 채운다.
  platformDefaultsLoaded: new Set(),
  // YouTube 카테고리 드롭다운에 실린 id. 기본값이 목록 밖이면 항목을 더한다.
  youtubeCategoryIds: [],
  // 마지막으로 알린 송출 방식 전환(status·시작 시각). 폴링마다 같은 알림을 반복하지 않는다.
  lastSwitchNotice: "",
};

document.addEventListener("DOMContentLoaded", () => {
  bindElements();
  initializeDefaults();
  initializeRuntimeNotice();
  bindEvents();
  applyPlatformSelection();
  resetRemoteStream();
  renderAuth();
  updatePeerUi();
  void loadCameras();
  void healthCheck({ quiet: true }).catch(() => null);
  void refreshSessions({ quiet: true }).catch(() => null);
  // Reference-face 상태는 RequireUser 뒤에 있으므로 로그인한 뒤에만 조회한다.
});

function bindElements() {
  for (const id of [
    "authState",
    "authEmail",
    "authPassword",
    "authDetail",
    "signInBtn",
    "signUpBtn",
    "signOutBtn",
    "verifyRow",
    "verifyCode",
    "verifyBtn",
    "healthState",
    "websocketState",
    "peerState",
    "serverUrl",
    "sessionLabel",
    "cameraSelect",
    "resolutionSelect",
    "sendAudio",
    "autoPoll",
    "startBtn",
    "goLiveBtn",
    "pauseBroadcastBtn",
    "changeResolutionBtn",
    "resumeBroadcastBtn",
    "stopBroadcastBtn",
    "healthBtn",
    "createSessionBtn",
    "refreshSessionsBtn",
    "errorProbeBtn",
    "disconnectBtn",
    "deleteSessionBtn",
    "broadcastSettingsState",
    "broadcastSettingsDetail",
    "saveBroadcastBtn",
    "runtimeNotice",
    "servedClientLink",
    "referenceFaceState",
    "referenceFaceDetail",
    "referenceFaceList",
    "referenceFaceInput",
    "referenceFacePreview",
    "referenceFacePreviewText",
    "uploadReferenceFaceBtn",
    "refreshReferenceFaceBtn",
    "deleteReferenceFaceBtn",
    "localTrackState",
    "remoteTrackState",
    "localVideo",
    "remoteVideo",
    "sessionId",
    "sessionStatus",
    "connectionState",
    "iceState",
    "signalingState",
    "aiFallback",
    "videoSenderActive",
    "ignoredTracks",
    "sessionToOffer",
    "offerToAnswer",
    "offerToConnected",
    "answerToConnected",
    "offerToIceDone",
    "answerToIceDone",
    "sessionCount",
    "sessionsList",
    "eventLog",
    "clearLogBtn",
    "copyJsonBtn",
    "sessionJson",
    "connectYoutubeBtn",
    "youtubeDetail",
    "connectChzzkBtn",
    "disconnectChzzkBtn",
    "disconnectYoutubeBtn",
    "loadBroadcastDefaults",
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
    "broadcastAccounts",
    "broadcastPlatformState",
    "broadcastVideoInput",
    "broadcastRtmpState",
    "loginView",
    "viewNav",
    "navStreamBtn",
    "navAdminBtn",
    "streamView",
    "adminView",
    "adminSessionCount",
    "adminRefreshSessionsBtn",
    "adminSessionsBody",
    "adminUserQuery",
    "adminSearchUsersBtn",
    "adminUsersBody",
    "adminDetail",
  ]) {
    els[id] = document.getElementById(id);
  }
}

function initializeDefaults() {
  els.serverUrl.value = readStoredServerUrl() || inferDefaultServerUrl();
  els.sessionLabel.value = `demo-${new Date().toISOString().slice(11, 19)}`;
}

function initializeRuntimeNotice() {
  const fileProtocol = location.protocol === "file:";
  els.runtimeNotice.hidden = !fileProtocol;
  updateServedClientLink();
}

function updateServedClientLink() {
  els.servedClientLink.href = apiUrl("/client/");
}

function bindEvents() {
  els.serverUrl.addEventListener("change", () => {
    persistServerUrl();
    updateServedClientLink();
    void healthCheck().catch(() => null);
    void refreshReferenceFace({ quiet: true }).catch(() => null);
  });
  els.signInBtn.addEventListener("click", () => void signIn());
  els.signUpBtn.addEventListener("click", () => void requestSignup());
  els.verifyBtn.addEventListener("click", () => void verifySignup());
  els.signOutBtn.addEventListener("click", () => void signOut());
  els.navStreamBtn.addEventListener("click", () => showView("stream"));
  els.navAdminBtn.addEventListener("click", () => showView("admin"));
  els.adminRefreshSessionsBtn.addEventListener("click", () => void refreshAdminSessions());
  els.adminSearchUsersBtn.addEventListener("click", () => void searchAdminUsers());
  els.adminUserQuery.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      void searchAdminUsers();
    }
  });
  els.connectYoutubeBtn.addEventListener("click", () => void connectYoutube());
  els.connectChzzkBtn.addEventListener("click", () => void connectChzzk());
  els.completeChzzkBtn.addEventListener("click", () => void completeChzzkConnect());
  els.disconnectChzzkBtn.addEventListener("click", () => void disconnectChzzk());
  els.disconnectYoutubeBtn.addEventListener("click", () => void disconnectYoutube());
  // 새로고침·탭 닫기면 세션을 지운다. 그대로 두면 서버가 망 복구를 기다리며 약
  // 50초 세션을 쥐고 있어 다시 시작할 때 409가 난다.
  window.addEventListener("pagehide", () => closeSessionOnPageHide());
  for (const provider of PLATFORMS) {
    platformToggle(provider).addEventListener("change", () => {
      applyPlatformSelection();
      // 세션이 있는 채로 새로 고른 플랫폼은 그 플랫폼의 직전 방송 값으로 채운다.
      if (platformToggle(provider).checked && state.session?.session_id) {
        void applyPlatformDefaults(state.session.session_id, provider);
      }
    });
  }
  els.broadcastResolution.addEventListener("change", () => {
    renderModeHint();
    updateButtons();
    void syncCaptureResolution();
  });
  els.chzzkCategorySearchBtn.addEventListener("click", () =>
    void searchChzzkCategories().catch(() => null),
  );
  els.chzzkCategoryResults.addEventListener("change", () => applyChzzkCategorySelection());
  els.authPassword.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      void signIn();
    }
  });
  els.healthBtn.addEventListener("click", () =>
    void healthCheck().catch(() => null),
  );
  els.createSessionBtn.addEventListener("click", () => void createSessionOnly());
  els.refreshSessionsBtn.addEventListener("click", () =>
    void refreshSessions().catch(() => null),
  );
  els.startBtn.addEventListener("click", () => void startWebRtc());
  els.goLiveBtn.addEventListener("click", () => void goLiveBroadcast());
  els.pauseBroadcastBtn.addEventListener("click", () => void pauseBroadcast());
  els.changeResolutionBtn.addEventListener("click", () => void changeBroadcastMode());
  els.youtubeApplyLiveBtn.addEventListener("click", () => void applyLiveSettings("youtube"));
  els.chzzkApplyLiveBtn.addEventListener("click", () => void applyLiveSettings("chzzk"));
  els.declineUpgradeBtn.addEventListener("click", () => void declineUpgradeOffer());
  els.confirmUpgradeBtn.addEventListener("click", () => void confirmUpgradeOption());
  els.resumeBroadcastBtn.addEventListener("click", () => void resumeBroadcast());
  els.stopBroadcastBtn.addEventListener("click", () => void stopBroadcast());
  els.disconnectBtn.addEventListener("click", () => void disconnect());
  els.deleteSessionBtn.addEventListener("click", () => void deleteCurrentSession());
  els.errorProbeBtn.addEventListener("click", () => sendErrorProbe());
  els.referenceFaceInput.addEventListener("change", updateReferenceFacePreview);
  els.uploadReferenceFaceBtn.addEventListener("click", () =>
    void uploadReferenceFace(),
  );
  els.refreshReferenceFaceBtn.addEventListener("click", () =>
    void refreshReferenceFace().catch(() => null),
  );
  els.deleteReferenceFaceBtn.addEventListener("click", () =>
    void deleteReferenceFace(),
  );
  els.saveBroadcastBtn.addEventListener("click", () =>
    void saveBroadcastSettings().catch(() => null),
  );
  // select·checkbox는 값이 늘 차 있어 "비었는지"로 사용자 입력을 가려낼 수
  // 없다 — 건드린 필드를 직접 표시해둔다.
  for (const provider of PLATFORMS) {
    for (const [field, element] of Object.entries(platformFormFields(provider))) {
      const markTouched = () => state.touchedBroadcastFields.add(`${provider}:${field}`);
      element.addEventListener("input", markTouched);
      element.addEventListener("change", markTouched);
    }
  }
  els.clearLogBtn.addEventListener("click", () => {
    els.eventLog.replaceChildren();
  });
  els.copyJsonBtn.addEventListener("click", () => void copySessionJson());
  els.autoPoll.addEventListener("change", () => {
    if (els.autoPoll.checked) {
      startPolling();
    } else {
      stopPolling();
    }
  });
}

function rememberSession(sessionId, ownerToken) {
  try {
    localStorage.setItem(LAST_SESSION_STORAGE_KEY, JSON.stringify({ session_id: sessionId, owner_token: ownerToken }));
  } catch {
    return;
  }
}

function forgetSession() {
  try {
    localStorage.removeItem(LAST_SESSION_STORAGE_KEY);
  } catch {
    return;
  }
}

function readRememberedSession() {
  try {
    const stored = JSON.parse(localStorage.getItem(LAST_SESSION_STORAGE_KEY) || "null");
    return stored?.session_id && stored?.owner_token ? stored : null;
  } catch {
    return null;
  }
}

// deleteRememberedSession은 보관해 둔 이전 세션을 지운다. 지울 세션이 있었으면
// true다(이미 끝나 404여도 자리는 비었다).
async function deleteRememberedSession() {
  const previous = readRememberedSession();
  if (!previous) {
    return false;
  }
  forgetSession();
  try {
    await apiFetch(`/sessions/${previous.session_id}`, {
      method: "DELETE",
      headers: { "X-Session-Owner-Token": previous.owner_token },
    });
  } catch (error) {
    if (error.status !== 404) {
      throw error;
    }
  }
  logEvent("warn", "Previous session removed", { session_id: previous.session_id });
  return true;
}

// closeSessionOnPageHide는 페이지를 떠날 때 세션 삭제를 보낸다. 응답은 기다릴 수
// 없으므로 keepalive로 보내고, 보관한 세션은 그대로 둔다 — 요청이 닿지 않았으면
// 다음 생성의 409 때 지운다.
function closeSessionOnPageHide() {
  const sessionId = state.session?.session_id;
  if (!sessionId || !state.ownerToken) {
    return;
  }
  const headers = { "X-Session-Owner-Token": state.ownerToken };
  if (state.accessToken) {
    headers.Authorization = `Bearer ${state.accessToken}`;
  }
  fetch(apiUrl(`/sessions/${sessionId}`), { method: "DELETE", headers, keepalive: true }).catch(() => null);
}

function inferDefaultServerUrl() {
  if (location.protocol === "http:" || location.protocol === "https:") {
    return location.origin;
  }
  return DEFAULT_SERVER_URL;
}

function readStoredServerUrl() {
  try {
    return localStorage.getItem(SERVER_STORAGE_KEY);
  } catch {
    return null;
  }
}

function persistServerUrl() {
  try {
    localStorage.setItem(SERVER_STORAGE_KEY, normalizeServerUrl(els.serverUrl.value));
  } catch {
    return;
  }
}

function getClientId() {
  try {
    const stored = localStorage.getItem(CLIENT_ID_STORAGE_KEY);
    if (stored) {
      return stored;
    }
    const generated = crypto.randomUUID
      ? crypto.randomUUID()
      : `client-${Date.now()}-${Math.random().toString(16).slice(2)}`;
    localStorage.setItem(CLIENT_ID_STORAGE_KEY, generated);
    return generated;
  } catch {
    return "web-client";
  }
}

function normalizeServerUrl(rawValue) {
  let value = String(rawValue || "").trim();
  if (!value) {
    value = inferDefaultServerUrl();
  }
  if (!/^https?:\/\//i.test(value)) {
    value = `https://${value}`;
  }
  return new URL(value).origin;
}

function apiUrl(path) {
  return new URL(path, normalizeServerUrl(els.serverUrl.value)).href;
}

function referenceFaceApiPath() {
  const params = new URLSearchParams({ client_id: getClientId() });
  return `/reference-face?${params.toString()}`;
}

function referenceFaceItemApiPath(faceId) {
  const params = new URLSearchParams({ client_id: getClientId() });
  return `/reference-face/${encodeURIComponent(faceId)}?${params.toString()}`;
}

function signalingUrl() {
  const url = new URL("/signaling", normalizeServerUrl(els.serverUrl.value));
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return url.href;
}

async function apiFetch(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (
    options.body !== undefined &&
    !(options.body instanceof FormData) &&
    !headers.has("Content-Type")
  ) {
    headers.set("Content-Type", "application/json");
  }
  // 사용자 식별: JWT access token은 활성 InnoLive 사용자를 증명하며 모든 세션과
  // reference-face 경로의 RequireUser에 필요하다.
  if (state.accessToken && !headers.has("Authorization")) {
    headers.set("Authorization", `Bearer ${state.accessToken}`);
  }
  // 세션 소유권: 세션 생성 때 한 번 발급한 owner token은 전용 header에 넣는다.
  // 세션 외 경로에 붙어도 문제는 없다.
  if (state.ownerToken && !headers.has("X-Session-Owner-Token")) {
    headers.set("X-Session-Owner-Token", state.ownerToken);
  }

  const response = await fetch(apiUrl(path), {
    ...options,
    headers,
  });
  const text = await response.text();
  const payload = parseJsonOrText(text);

  if (!response.ok) {
    // 401은 access token이 없거나 만료됐다는 뜻이다. 저장한 refresh token으로
    // 한 번 조용히 갱신한 뒤 요청을 재시도하고, 그것도 실패할 때만 세션을
    // 제거해 UI가 다시 로그인을 요구하게 한다.
    if (response.status === 401 && !options._retried && state.refreshToken) {
      if (await refreshAccessToken()) {
        return apiFetch(path, { ...options, _retried: true });
      }
    }
    if (response.status === 401 && state.accessToken) {
      clearAuthState();
      logEvent("warn", "Access token rejected; please sign in again.");
    }
    const message =
      payload?.error?.message || `${response.status} ${response.statusText}`;
    const error = new Error(message);
    error.status = response.status;
    error.payload = payload;
    throw error;
  }

  return payload;
}

function parseJsonOrText(text) {
  if (!text) {
    return null;
  }
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

async function refreshReferenceFace({ quiet = false } = {}) {
  setPill(els.referenceFaceState, "확인 중", "warn");
  try {
    const status = await apiFetch(referenceFaceApiPath());
    state.referenceFaceSupported = true;
    state.referenceFace = status;
    renderReferenceFaceStatus();
    if (!quiet) {
      logEvent("ok", "Reference face status refreshed", status);
    }
    return status;
  } catch (error) {
    if (error.status === 404) {
      state.referenceFaceSupported = false;
      state.referenceFace = null;
      setPill(els.referenceFaceState, "미지원", "idle");
      els.referenceFaceDetail.textContent =
        "연결한 서버에는 기준 얼굴 API가 배포되지 않았습니다.";
      if (!quiet) {
        logEvent("warn", "Reference face API is not supported by this server");
      }
      return null;
    }
    state.referenceFaceSupported = null;
    state.referenceFace = null;
    setPill(els.referenceFaceState, "조회 실패", "error");
    els.referenceFaceDetail.textContent = error.message;
    logError("Failed to refresh reference face status", error);
    throw error;
  } finally {
    updateButtons();
  }
}

function updateReferenceFacePreview() {
  if (state.referenceFacePreviewUrl) {
    URL.revokeObjectURL(state.referenceFacePreviewUrl);
    state.referenceFacePreviewUrl = null;
  }

  const [file] = els.referenceFaceInput.files;
  if (!file) {
    els.referenceFacePreview.hidden = true;
    els.referenceFacePreview.removeAttribute("src");
    els.referenceFacePreviewText.textContent = "JPEG, PNG, WebP · 파일당 최대 10MB";
    updateButtons();
    return;
  }

  state.referenceFacePreviewUrl = URL.createObjectURL(file);
  els.referenceFacePreview.src = state.referenceFacePreviewUrl;
  els.referenceFacePreview.alt = `${file.name} 미리보기`;
  els.referenceFacePreview.hidden = false;
  const files = Array.from(els.referenceFaceInput.files);
  els.referenceFacePreviewText.textContent = files.length === 1
    ? `${file.name} · ${formatBytes(file.size)}`
    : `${files.length}개 파일 · 첫 파일 ${formatBytes(file.size)}`;
  updateButtons();
}

async function uploadReferenceFace() {
  const files = Array.from(els.referenceFaceInput.files);
  if (!files.length) {
    return;
  }

  await runReferenceFaceBusy(async () => {
    const form = new FormData();
    form.append("client_id", getClientId());
    for (const file of files) {
      form.append("images", file, file.name);
    }
    const status = await apiFetch(referenceFaceApiPath(), {
      method: "POST",
      body: form,
    });
    state.referenceFace = status;
    renderReferenceFaceStatus();
    logEvent("ok", "Reference face registered", {
      files: files.map((file) => ({ name: file.name, size: file.size })),
      status,
    });
  });
}

async function deleteReferenceFace() {
  await runReferenceFaceBusy(async () => {
    await apiFetch(referenceFaceApiPath(), { method: "DELETE" });
    els.referenceFaceInput.value = "";
    updateReferenceFacePreview();
    await refreshReferenceFace({ quiet: true });
    logEvent("ok", "Reference face registration removed");
  });
}

async function deleteReferenceFaceById(faceId) {
  await runReferenceFaceBusy(async () => {
    await apiFetch(referenceFaceItemApiPath(faceId), { method: "DELETE" });
    await refreshReferenceFace({ quiet: true });
    logEvent("ok", "Reference face removed", { face_id: faceId });
  });
}

async function runReferenceFaceBusy(task) {
  if (state.referenceFaceBusy) {
    return;
  }
  state.referenceFaceBusy = true;
  updateButtons();
  try {
    await task();
  } catch (error) {
    logError("Reference face request failed", error);
  } finally {
    state.referenceFaceBusy = false;
    updateButtons();
  }
}

function renderReferenceFaceStatus() {
  const status = state.referenceFace;
  if (!status?.registered) {
    setPill(els.referenceFaceState, "미등록", "idle");
    els.referenceFaceDetail.textContent =
      "현재 모든 감지된 얼굴이 블러 처리됩니다.";
    renderReferenceFaceList([]);
    updateButtons();
    return;
  }

  setPill(els.referenceFaceState, "등록됨", "ok");
  const source = status.source === "env" ? "서버 설정" : "업로드";
  const countText = status.count ? `${status.count}장` : "사진 수 정보 없음";
  const registeredAt = status.source === "env"
    ? "클라이언트에서 해제할 수 없음"
    : status.registered_at
      ? new Date(status.registered_at).toLocaleString()
      : "시간 정보 없음";
  els.referenceFaceDetail.textContent = `${source} · ${countText} · ${registeredAt}`;
  renderReferenceFaceList(status.faces || []);
  updateButtons();
}

function renderReferenceFaceList(faces) {
  els.referenceFaceList.replaceChildren();
  if (!faces.length) {
    return;
  }

  for (const face of faces) {
    const row = document.createElement("div");
    row.className = "reference-face-item";

    const label = document.createElement("span");
    const registeredAt = face.registered_at
      ? new Date(face.registered_at).toLocaleString()
      : "시간 정보 없음";
    label.textContent = `${face.face_id.slice(0, 8)} · ${registeredAt}`;

    const button = document.createElement("button");
    button.className = "button button-danger button-small";
    button.type = "button";
    button.textContent = "삭제";
    button.disabled = state.referenceFaceBusy;
    button.addEventListener("click", () => void deleteReferenceFaceById(face.face_id));

    row.append(label, button);
    els.referenceFaceList.append(row);
  }
}

function formatBytes(bytes) {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

async function healthCheck({ quiet = false } = {}) {
  setPill(els.healthState, "HTTP checking", "warn");
  try {
    const payload = await apiFetch("/");
    if (!payload || payload.service !== "inno-live-server") {
      throw new Error("Server root did not return the inno-live-server health payload.");
    }
    setPill(els.healthState, "HTTP ok", "ok");
    if (!quiet) {
      logEvent("ok", "Health check succeeded", payload);
    }
    return payload;
  } catch (error) {
    setPill(els.healthState, "HTTP error", "error");
    logError("Health check failed", error);
    throw error;
  } finally {
    updateButtons();
  }
}

async function signIn() {
  const email = els.authEmail.value.trim();
  const password = els.authPassword.value;
  if (!email || !password) {
    els.authDetail.textContent = "이메일과 비밀번호를 모두 입력하세요.";
    return;
  }
  await runBusy(async () => {
    persistServerUrl();
    setPill(els.authState, "Auth checking", "warn");
    try {
      const pair = await apiFetch("/auth/sign-in", {
        method: "POST",
        body: JSON.stringify({ email, password }),
      });
      state.accessToken = pair?.access_token || null;
      state.refreshToken = pair?.refresh_token || null;
      state.authEmail = email;
      els.authPassword.value = "";
      state.isAdmin = await checkAdminAccess();
      if (!state.isAdmin) {
        clearAuthState();
        els.authDetail.textContent = "관리자 계정만 사용할 수 있습니다.";
        logEvent("warn", "Signed in user is not an administrator", { email });
        return;
      }
      renderAuth();
      logEvent("ok", "Signed in", { email, expires_in: pair?.expires_in });
      await refreshReferenceFace({ quiet: true }).catch(() => null);
      await afterSignIn();
    } catch (error) {
      clearAuthState();
      // runBusy가 실패를 기록하므로, 한 번만 표시되게 다시 던진다.
      throw error;
    }
  });
}

// requestSignup은 이메일 가입 절차를 시작한다. 서버는 인증 코드를 이메일로 보내고
// 짝이 되는 signup_token을 반환한다. native endpoint는 browser 테스트 클라이언트가
// cookie에 의존하지 않도록 token을 body에 넣어 반환한다.
async function requestSignup() {
  const email = els.authEmail.value.trim();
  const password = els.authPassword.value;
  if (!email || !password) {
    els.authDetail.textContent = "회원가입하려면 이메일과 비밀번호를 입력하세요.";
    return;
  }
  await runBusy(async () => {
    try {
      const res = await apiFetch("/auth/native/sign-up", {
        method: "POST",
        body: JSON.stringify({ email, password }),
      });
      state.signupToken = res?.signup_token || null;
      renderAuth();
      els.authDetail.textContent = `${email} 로 인증 코드를 보냈습니다. 메일의 코드를 입력하고 "인증 완료"를 누르세요.`;
      logEvent("ok", "Signup verification code sent", { email });
    } catch (error) {
      els.authDetail.textContent = `회원가입 실패: ${error.message}`;
      throw error; // runBusy가 한 번 기록한다.
    }
  });
}

// verifySignup은 이메일 인증 코드를 확인한 뒤 같은 credentials로 로그인해,
// 테스트 사용자가 한 단계로 인증된 상태에 도달하게 한다.
async function verifySignup() {
  const code = els.verifyCode.value.trim();
  if (!state.signupToken || !code) {
    els.authDetail.textContent = "메일로 받은 인증 코드를 입력하세요.";
    return;
  }
  await runBusy(async () => {
    try {
      await apiFetch("/auth/native/verify-email", {
        method: "POST",
        body: JSON.stringify({
          signup_token: state.signupToken,
          verification_code: code,
        }),
      });
      state.signupToken = null;
      els.verifyCode.value = "";
      renderAuth();
      els.authDetail.textContent = "가입 완료! 로그인합니다...";
      logEvent("ok", "Email verified");
    } catch (error) {
      els.authDetail.textContent = `인증 실패: ${error.message}`;
      throw error; // runBusy가 한 번 기록한다.
    }
  });
  if (!state.signupToken) {
    await signIn();
  }
}

async function signOut() {
  // 연결이 속한 세션을 제거하기 전에 실행 중인 연결을 종료해, 세션 중간 로그아웃이
  // peer connection을 남기지 않게 한다.
  await cleanupConnection({ keepSession: false });
  clearAuthState();
  logEvent("ok", "Signed out");
}

// checkAdminAccess는 로그인한 사용자가 관리자인지 서버에 묻는다(#373). 403이면
// 관리자가 아니고, 404면 관리자 라우트가 조립되지 않은 서버(DB 없음)다.
async function checkAdminAccess() {
  try {
    await apiFetch("/admin/me");
    return true;
  } catch (error) {
    if (error.status === 403 || error.status === 404) {
      return false;
    }
    throw error;
  }
}

function clearAuthState() {
  state.accessToken = null;
  state.refreshToken = null;
  state.authEmail = null;
  state.isAdmin = false;
  state.view = "stream";
  state.signupToken = null;
  els.verifyCode.value = "";
  renderAuth();
}

// refreshAccessToken은 저장한 refresh token을 새 access token으로 교환한다.
// /auth/refresh의 401이 refresh를 재귀 호출하지 않도록 apiFetch가 아닌 fetch를
// 직접 호출하며, 공유 promise로 동시 호출을 합친다(2초 세션 poll이 동시에 여러
// 401을 만들 수 있다).
async function refreshAccessToken() {
  if (!state.refreshToken) {
    return false;
  }
  if (!state.refreshPromise) {
    state.refreshPromise = (async () => {
      try {
        const response = await fetch(apiUrl("/auth/refresh"), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ refresh_token: state.refreshToken }),
        });
        if (!response.ok) {
          clearAuthState();
          return false;
        }
        const pair = await response.json();
        state.accessToken = pair?.access_token || null;
        state.refreshToken = pair?.refresh_token || state.refreshToken;
        renderAuth();
        logEvent("ok", "Access token refreshed");
        return Boolean(state.accessToken);
      } catch {
        return false;
      } finally {
        state.refreshPromise = null;
      }
    })();
  }
  return state.refreshPromise;
}

// renderAuth는 세 가지 인증 모드를 반영해 각 모드의 control만 표시한다. 인증 코드
// 필드는 이메일 코드를 입력할 때만 보이며 로그인 화면이나 코드 첫 요청 때는 보이지
// 않는다.
function renderAuth() {
  const signedIn = Boolean(state.accessToken);
  const verifying = Boolean(state.signupToken);
  // 로그인 화면은 관리자로 들어가기 전까지만, 들어간 뒤에는 고른 화면 하나만 편다.
  const entered = signedIn && state.isAdmin;
  els.loginView.hidden = entered;
  els.viewNav.hidden = !entered;
  els.streamView.hidden = !entered || state.view !== "stream";
  els.adminView.hidden = !entered || state.view !== "admin";
  setCurrentTab(els.navStreamBtn, state.view === "stream");
  setCurrentTab(els.navAdminBtn, state.view === "admin");
  setPill(els.authState, signedIn ? "Auth ok" : "Auth idle", signedIn ? "ok" : "idle");
  els.signInBtn.hidden = signedIn || verifying;
  els.signUpBtn.hidden = signedIn || verifying;
  els.verifyRow.hidden = !verifying;
  els.verifyBtn.hidden = !verifying;
  els.signOutBtn.hidden = !signedIn;
  // YouTube 연결은 로그인(이메일)과 별개의 부가 기능이다 — 로그인 상태에서만 노출.
  els.connectYoutubeBtn.hidden = !signedIn;
  if (!signedIn) {
    els.disconnectYoutubeBtn.hidden = true;
  }
  els.connectChzzkBtn.hidden = !signedIn;
  if (!signedIn) {
    els.youtubeDetail.hidden = true;
    state.usage = null;
    els.planSummary.hidden = true;
    resetChzzkUi();
    resetBroadcastStatus();
  }
  els.authDetail.textContent = signedIn
    ? `${state.authEmail} 로 로그인됨. 세션 API를 사용할 수 있습니다.`
    : verifying
      ? '메일로 받은 인증 코드를 입력하고 "인증 완료"를 누르세요.'
      : "관리자 계정으로 로그인하세요. 계정이 없으면 이메일·비밀번호 입력 후 회원가입한 뒤 관리자 등록을 요청하세요.";
  updateButtons();
}

function setCurrentTab(button, current) {
  if (current) {
    button.setAttribute("aria-current", "page");
  } else {
    button.removeAttribute("aria-current");
  }
}

function showView(view) {
  state.view = view;
  renderAuth();
  if (view === "admin") {
    void refreshAdminSessions();
    void searchAdminUsers();
  }
}

const ADMIN_PLANS = ["spark", "glow", "beam", "plasma"];

// refreshAdminSessions는 모든 사용자의 활성 세션을 표로 그린다(#373).
async function refreshAdminSessions() {
  try {
    const payload = await apiFetch("/admin/sessions");
    renderAdminSessions(payload?.sessions || []);
  } catch (error) {
    setAdminDetail(`세션 목록을 불러오지 못했습니다: ${error.message}`);
  }
}

function renderAdminSessions(sessions) {
  els.adminSessionCount.textContent = String(sessions.length);
  if (sessions.length === 0) {
    els.adminSessionsBody.replaceChildren(adminEmptyRow(7, "활성 세션이 없습니다."));
    return;
  }
  els.adminSessionsBody.replaceChildren(
    ...sessions.map((item) => {
      const closeBtn = document.createElement("button");
      closeBtn.type = "button";
      closeBtn.className = "button button-danger button-small";
      closeBtn.textContent = "강제 종료";
      closeBtn.addEventListener("click", () => void closeAdminSession(item));
      return adminRow([
        item.guest ? "게스트" : item.email || item.user_id,
        item.plan || "-",
        item.status,
        adminTargetsText(item.targets),
        item.created_at ? new Date(item.created_at).toLocaleString() : "-",
        String(item.session_id).slice(0, 8),
        closeBtn,
      ]);
    }),
  );
}

function adminTargetsText(targets) {
  const active = (targets || []).filter((target) => target?.stream?.broadcast_phase !== "idle");
  if (active.length === 0) {
    return "-";
  }
  return active.map((target) => `${target.provider} ${target.stream?.status || ""}`.trim()).join(", ");
}

// closeAdminSession은 확인을 받은 뒤 세션을 강제로 끝낸다. 송출 중이면 방송도 끝난다.
async function closeAdminSession(item, confirmClose = (message) => window.confirm(message)) {
  const who = item.guest ? "게스트" : item.email || item.user_id;
  if (!confirmClose(`${who}의 세션을 종료합니다. 송출 중이면 방송도 끝납니다. 계속할까요?`)) {
    return;
  }
  try {
    await apiFetch(`/admin/sessions/${encodeURIComponent(item.session_id)}`, { method: "DELETE" });
    setAdminDetail(`${who}의 세션을 종료했습니다.`);
    logEvent("ok", "Session closed by admin", { session_id: item.session_id });
  } catch (error) {
    setAdminDetail(`세션을 종료하지 못했습니다: ${error.message}`);
  }
  await refreshAdminSessions();
}

// searchAdminUsers는 이메일로 사용자를 찾아 플랜 변경 표를 그린다.
async function searchAdminUsers() {
  const query = els.adminUserQuery.value.trim();
  try {
    const payload = await apiFetch(`/admin/users?email=${encodeURIComponent(query)}`);
    renderAdminUsers(payload?.users || []);
  } catch (error) {
    setAdminDetail(`사용자를 불러오지 못했습니다: ${error.message}`);
  }
}

function renderAdminUsers(users) {
  if (users.length === 0) {
    els.adminUsersBody.replaceChildren(adminEmptyRow(3, "일치하는 사용자가 없습니다."));
    return;
  }
  els.adminUsersBody.replaceChildren(
    ...users.map((user) => {
      const select = document.createElement("select");
      for (const value of ADMIN_PLANS) {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = value;
        select.append(option);
      }
      select.value = user.plan;
      const applyBtn = document.createElement("button");
      applyBtn.type = "button";
      applyBtn.className = "button button-small";
      applyBtn.textContent = "적용";
      applyBtn.addEventListener("click", () => void changeUserPlan(user, select.value));
      const control = document.createElement("span");
      control.className = "button-row";
      control.append(select, applyBtn);
      return adminRow([user.email || user.user_id, user.plan, control]);
    }),
  );
}

// changeUserPlan은 사용자 플랜을 바꾼다. 진행 중인 세션에는 반영되지 않는다.
async function changeUserPlan(user, value) {
  try {
    await apiFetch(`/admin/users/${encodeURIComponent(user.user_id)}/plan`, {
      method: "PUT",
      body: JSON.stringify({ plan: value }),
    });
    setAdminDetail(`${user.email || user.user_id}의 플랜을 ${value}로 바꿨습니다. 새 세션부터 적용됩니다.`);
    logEvent("ok", "User plan changed by admin", { user_id: user.user_id, plan: value });
  } catch (error) {
    setAdminDetail(`플랜을 바꾸지 못했습니다: ${error.message}`);
    return;
  }
  await searchAdminUsers();
  if (user.email && user.email === state.authEmail) {
    await refreshPlan();
  }
}

function adminRow(cells) {
  const row = document.createElement("tr");
  for (const value of cells) {
    const cell = document.createElement("td");
    if (typeof value === "string") {
      cell.textContent = value;
    } else {
      cell.append(value);
    }
    row.append(cell);
  }
  return row;
}

function adminEmptyRow(span, text) {
  const row = document.createElement("tr");
  const cell = document.createElement("td");
  cell.colSpan = span;
  cell.className = "admin-empty";
  cell.textContent = text;
  row.append(cell);
  return row;
}

function setAdminDetail(text) {
  els.adminDetail.textContent = text;
}

function setYoutubeDetail(text, isError) {
  els.youtubeDetail.hidden = false;
  els.youtubeDetail.textContent = text;
  els.youtubeDetail.style.color = isError ? "var(--danger, #b00020)" : "";
}

function setBroadcastStatus(element, text, visualState = "idle") {
  element.textContent = text;
  element.dataset.state = visualState;
}

function resetBroadcastStatus() {
  setBroadcastStatus(els.broadcastAccounts, "연결 전");
  setBroadcastStatus(els.broadcastVideoInput, "WebRTC 시작 전");
  setBroadcastStatus(els.broadcastRtmpState, "시작 전");
  setBroadcastStatus(els.broadcastPlatformState, "확인 전");
  setBroadcastStatus(els.broadcastSettingsState, "저장 전");
  els.broadcastSettingsDetail.textContent = "세션을 만든 뒤 저장할 수 있습니다.";
}

function renderBroadcastStreamStatus(stream) {
  const status = stream?.status || "";
  const attempts = Number(stream?.reconnect_attempts || 0);
  const reconnectDetail = attempts > 0 ? ` · ${attempts}회 재시도` : "";
  if (!status) {
    setBroadcastStatus(els.broadcastRtmpState, "시작 전");
    setBroadcastStatus(els.broadcastPlatformState, "확인 전");
    return;
  }

  if (status === "streaming") {
    setBroadcastStatus(els.broadcastRtmpState, "송출 중", "ok");
    // broadcast_phase는 egress가 알 수 없는 YouTube 쪽 위치다(#142) —
    // 송출 중이어도 라이브 전환 전이면 시청자에게 보이지 않는다.
    if (stream?.broadcast_phase === "live") {
      setBroadcastStatus(els.broadcastPlatformState, "라이브 중", "ok");
    } else if (stream?.broadcast_phase === "going_live") {
      setBroadcastStatus(els.broadcastPlatformState, "라이브 전환 중", "warn");
    } else {
      setBroadcastStatus(els.broadcastPlatformState, "준비됨 · 라이브 전환 대기", "warn");
    }
    return;
  }
  if (status === "reconfiguring") {
    setBroadcastStatus(els.broadcastRtmpState, "입력 규격 변경 중", "warn");
    setBroadcastStatus(els.broadcastPlatformState, "RTMP 재구성 중", "warn");
    return;
  }
  if (status === "idle" || status === "reconnecting") {
    setBroadcastStatus(
      els.broadcastRtmpState,
      status === "reconnecting" ? `재연결 중${reconnectDetail}` : "연결 준비 중",
      "warn",
    );
    setBroadcastStatus(
      els.broadcastPlatformState,
      status === "reconnecting" ? "RTMP 재연결 대기" : "RTMP 연결 대기",
      "warn",
    );
    return;
  }
  if (status === "paused") {
    setBroadcastStatus(els.broadcastRtmpState, "일시 중지됨", "warn");
    setBroadcastStatus(els.broadcastPlatformState, "RTMP 연결 유지 중", "warn");
    return;
  }
  if (status === "paused_reconfiguring") {
    setBroadcastStatus(els.broadcastRtmpState, "일시 중지 준비 중", "warn");
    setBroadcastStatus(els.broadcastPlatformState, "새 규격으로 RTMP 재구성 중", "warn");
    return;
  }
  if (status === "paused_reconnecting") {
    setBroadcastStatus(els.broadcastRtmpState, `일시 중지 화면 재연결 중${reconnectDetail}`, "warn");
    setBroadcastStatus(els.broadcastPlatformState, "RTMP 재연결 중", "warn");
    return;
  }
  if (status === "stopped") {
    if (stream?.stop_reason === "rtmp_reconnect_exhausted") {
      setBroadcastStatus(els.broadcastRtmpState, "RTMP 재연결 실패로 종료됨", "error");
      setBroadcastStatus(els.broadcastPlatformState, "다시 송출할 수 있습니다", "error");
      return;
    }
    if (stream?.stop_reason === "reconnect_input_timeout") {
      setBroadcastStatus(els.broadcastRtmpState, "입력 프레임 대기 시간 초과로 종료됨", "error");
      setBroadcastStatus(els.broadcastPlatformState, "다시 송출할 수 있습니다", "error");
      return;
    }
    setBroadcastStatus(els.broadcastRtmpState, "중지됨");
    setBroadcastStatus(els.broadcastPlatformState, "종료 반영 대기");
    return;
  }
  setBroadcastStatus(els.broadcastRtmpState, status, "warn");
  setBroadcastStatus(els.broadcastPlatformState, "상태 확인 중", "warn");
}

// connectYoutube는 GIS 팝업으로 인가 코드를 받아 서버에 전달해 YouTube 계정을
// 연결한다. 로그인 자체는 이메일 그대로이고, 이 팝업은 송출 대상 연결 전용이다.
// 코드 교환·토큰 보관은 전부 서버 몫이라 브라우저에는 인가 코드만 스친다.
async function connectYoutube() {
  if (!state.accessToken) {
    setBroadcastStatus(els.broadcastAccounts, "로그인 필요", "error");
    setYoutubeDetail("먼저 로그인하세요.", true);
    return;
  }
  if (!window.google?.accounts?.oauth2) {
    setBroadcastStatus(els.broadcastAccounts, "연결 실패", "error");
    setYoutubeDetail("Google 스크립트를 아직 불러오지 못했습니다. 잠시 후 다시 시도하세요.", true);
    return;
  }
  let config;
  try {
    config = await apiFetch("/auth/youtube/config");
  } catch (error) {
    setBroadcastStatus(els.broadcastAccounts, "연결 설정 실패", "error");
    setYoutubeDetail(`서버에서 YouTube 연동 설정을 받지 못했습니다: ${error.message}`, true);
    return;
  }
  setBroadcastStatus(els.broadcastAccounts, "연결 중", "warn");
  setYoutubeDetail("Google 팝업에서 계정을 선택하고 동의해 주세요...");
  const codeClient = window.google.accounts.oauth2.initCodeClient({
    client_id: config.web_client_id,
    scope: config.scope,
    ux_mode: "popup",
    callback: (response) => {
      if (!response.code) {
        setBroadcastStatus(els.broadcastAccounts, "연결 실패", "error");
        setYoutubeDetail("Google이 인가 코드를 돌려주지 않았습니다.", true);
        return;
      }
      void (async () => {
        try {
          const result = await apiFetch("/auth/youtube/connect", {
            method: "POST",
            body: JSON.stringify({
              server_auth_code: response.code,
              code_source: "web_popup",
            }),
          });
          const title = result?.channel?.title || result?.channel?.id || "알 수 없는 채널";
          setYoutubeDetail(`YouTube 연결됨: ${title}`);
          logEvent("ok", "YouTube account connected", result);
          await refreshStreamingAccounts().catch(() => null);
        } catch (error) {
          setBroadcastStatus(els.broadcastAccounts, "연결 실패", "error");
          setYoutubeDetail(`연결 실패: ${error.message}`, true);
          logEvent("error", "YouTube connect failed", { message: error.message });
        }
      })();
    },
    error_callback: (error) => {
      setBroadcastStatus(els.broadcastAccounts, "연결 실패", "error");
      setYoutubeDetail(`Google 팝업 오류: ${error?.type || JSON.stringify(error)}`, true);
    },
  });
  codeClient.requestCode();
}

// 치지직 연결은 GIS 같은 팝업 SDK가 없는 순수 리다이렉트 흐름이다. 등록된
// redirect_uri에 아직 페이지가 없어(404) 코드가 자동으로 돌아오지 않으므로,
// 사용자가 동의 후 이동한 주소를 붙여넣어 완료한다. state는 여기서 만들고
// 여기서 대조한다 — 서버는 보관하지 않는다(docs/api/AUTHENTICATION.md).
function resetChzzkUi() {
  state.chzzkState = null;
  els.chzzkCallbackRow.hidden = true;
  els.chzzkCompleteRow.hidden = true;
  els.chzzkCallbackUrl.value = "";
  els.chzzkDetail.hidden = true;
  els.disconnectChzzkBtn.hidden = true;
}

function setChzzkDetail(text, isError) {
  els.chzzkDetail.hidden = false;
  els.chzzkDetail.textContent = text;
  els.chzzkDetail.style.color = isError ? "var(--danger, #b00020)" : "";
}

async function connectChzzk() {
  if (!state.accessToken) {
    setChzzkDetail("먼저 로그인하세요.", true);
    return;
  }
  const chzzkState = crypto.randomUUID();
  let config;
  try {
    config = await apiFetch(`/auth/chzzk/config?state=${encodeURIComponent(chzzkState)}`);
  } catch (error) {
    setChzzkDetail(`서버에서 치지직 연동 설정을 받지 못했습니다: ${error.message}`, true);
    return;
  }
  if (!config?.authorize_url) {
    setChzzkDetail("서버가 authorize_url을 돌려주지 않았습니다.", true);
    return;
  }
  state.chzzkState = chzzkState;
  els.chzzkCallbackRow.hidden = false;
  els.chzzkCompleteRow.hidden = false;
  els.chzzkCallbackUrl.value = "";
  // config 응답을 기다린 뒤라 클릭 활성화가 끝나 팝업이 막힐 수 있다 — 그때는
  // 링크로 대신한다.
  const opened = window.open(config.authorize_url, "_blank", "noopener");
  if (opened) {
    setChzzkDetail("새 창에서 동의한 뒤, 이동한 페이지의 주소를 붙여넣고 '치지직 연결 완료'를 누르세요.");
    return;
  }
  els.chzzkDetail.hidden = false;
  els.chzzkDetail.style.color = "";
  els.chzzkDetail.textContent = "팝업이 차단됐습니다. 이 링크로 동의한 뒤 주소를 붙여넣으세요: ";
  const link = document.createElement("a");
  link.href = config.authorize_url;
  link.target = "_blank";
  link.rel = "noopener";
  link.textContent = "치지직 인가 페이지";
  els.chzzkDetail.append(link);
}

// parseChzzkCallback은 붙여넣은 콜백 주소에서 code·state를 꺼낸다. URL이 아니면
// null — code만 붙여넣는 경로는 state 대조가 불가능하므로 받지 않는다.
function parseChzzkCallback(raw) {
  let url;
  try {
    url = new URL(raw.trim());
  } catch {
    return null;
  }
  const code = url.searchParams.get("code") || "";
  const returnedState = url.searchParams.get("state") || "";
  if (!code || !returnedState) {
    return null;
  }
  return { code, state: returnedState };
}

async function completeChzzkConnect() {
  if (!state.chzzkState) {
    setChzzkDetail("먼저 '치지직 계정 연결'로 인가를 시작하세요.", true);
    return;
  }
  const parsed = parseChzzkCallback(els.chzzkCallbackUrl.value);
  if (!parsed) {
    setChzzkDetail("code와 state가 있는 콜백 URL 전체를 붙여넣으세요.", true);
    return;
  }
  if (parsed.state !== state.chzzkState) {
    setChzzkDetail("state가 인가 요청과 다릅니다. 연결을 다시 시작하세요.", true);
    logEvent("error", "Chzzk state mismatch");
    return;
  }
  try {
    const result = await apiFetch("/auth/chzzk/connect", {
      method: "POST",
      body: JSON.stringify({ code: parsed.code, state: parsed.state }),
    });
    const title = result?.channel?.channelName || result?.channel?.channelId || "알 수 없는 채널";
    state.chzzkState = null;
    els.chzzkCallbackRow.hidden = true;
    els.chzzkCompleteRow.hidden = true;
    els.chzzkCallbackUrl.value = "";
    setChzzkDetail(`치지직 연결됨: ${title}`);
    logEvent("ok", "Chzzk account connected", result);
  } catch (error) {
    setChzzkDetail(`연결 실패: ${error.message}`, true);
    logEvent("error", "Chzzk connect failed", { message: error.message });
    return;
  }
  // 연결은 이미 끝났다 — 목록 갱신 실패를 연결 실패로 보이게 하지 않는다.
  await refreshStreamingAccounts().catch(() => null);
}

// refreshStreamingAccounts는 연결된 플랫폼 계정을 읽어 방송 상태에 표시하고, 치지직
// 연결 UI와 YouTube 카테고리 목록을 맞춘다.
async function refreshStreamingAccounts() {
  const accounts = await apiFetch("/auth/streaming/accounts");
  if (!state.accessToken) {
    return; // 응답을 기다리는 사이 로그아웃됐다.
  }
  const list = Array.isArray(accounts) ? accounts : [];
  const describe = (account) =>
    `${platformLabel(account.provider)}: ${account.channel_title || account.channel_id || "연결됨"}` +
    (account.reconnect_required ? " (재연결 필요)" : "");
  setBroadcastStatus(
    els.broadcastAccounts,
    list.length ? list.map(describe).join(" · ") : "연결된 계정 없음",
    list.length ? "ok" : "warn",
  );
  const chzzk = list.find((account) => account?.provider === "chzzk");
  els.disconnectChzzkBtn.hidden = !chzzk;
  els.disconnectYoutubeBtn.hidden = !list.some((account) => account?.provider === "youtube");
  if (chzzk) {
    const title = chzzk.channel_title || chzzk.channel_id || "알 수 없는 채널";
    setChzzkDetail(
      chzzk.reconnect_required ? `치지직 연결됨: ${title} (재연결 필요)` : `치지직 연결됨: ${title}`,
      Boolean(chzzk.reconnect_required),
    );
  }
  if (list.some((account) => account?.provider === "youtube")) {
    await loadYoutubeCategories();
  }
}

async function disconnectYoutube() {
  try {
    await apiFetch("/auth/streaming/accounts/youtube", { method: "DELETE" });
    els.disconnectYoutubeBtn.hidden = true;
    logEvent("ok", "YouTube account disconnected");
    await refreshStreamingAccounts().catch(() => null);
  } catch (error) {
    logEvent("error", "YouTube disconnect failed", { message: disconnectErrorMessage(error) });
  }
}

// disconnectErrorMessage는 연결 해제 실패 안내다. 방송 중인 플랫폼은 서버가 409로
// 거절한다(#348).
function disconnectErrorMessage(error) {
  return error.payload?.error?.code === "streaming_account_in_use"
    ? "이 플랫폼으로 방송 중에는 연결을 해제할 수 없습니다. 방송을 먼저 종료하세요."
    : error.message;
}

async function disconnectChzzk() {
  try {
    await apiFetch("/auth/streaming/accounts/chzzk", { method: "DELETE" });
    els.disconnectChzzkBtn.hidden = true;
    setChzzkDetail("치지직 연결을 해제했습니다.");
    logEvent("ok", "Chzzk account disconnected");
  } catch (error) {
    setChzzkDetail(`해제 실패: ${disconnectErrorMessage(error)}`, true);
    logEvent("error", "Chzzk disconnect failed", { message: disconnectErrorMessage(error) });
  }
}

// ── 송출 구성(#302) ───────────────────────────────────────────────
// 플랫폼은 동등한 카드다. 고른 플랫폼마다 prepare를 한 번씩 부르고 golive를 한 번
// 부르는 것이 곧 단독·동시 송출이다(#233). 순서는 고정(YouTube, 치지직)이고 첫
// 번째가 세션의 기본 대상이 된다 — 서버는 세션마다 기본 대상 하나를 요구한다.
const PLATFORMS = ["youtube", "chzzk"];
const PLATFORM_LABELS = { youtube: "YouTube", chzzk: "치지직" };
const MODE_LABELS = {
  "720p_single": "720p 단독",
  fhd_single: "FHD 단독",
  "720p_multi": "720p 동시",
  fhd_multi: "FHD 동시",
};

function platformLabel(provider) {
  return PLATFORM_LABELS[provider] || provider;
}

function platformToggle(provider) {
  return provider === "chzzk" ? els.platformChzzk : els.platformYoutube;
}

function selectedPlatforms() {
  return PLATFORMS.filter((provider) => platformToggle(provider).checked);
}

// applyPlatformSelection은 고른 카드만 상세 설정을 펴고 송출 방식 안내를 갱신한다.
function applyPlatformSelection() {
  for (const provider of PLATFORMS) {
    const selected = platformToggle(provider).checked;
    (provider === "chzzk" ? els.chzzkSettings : els.youtubeSettings).hidden = !selected;
    (provider === "chzzk" ? els.chzzkCard : els.youtubeCard).dataset.selected = String(selected);
  }
  renderModeHint();
  updateButtons();
}

function broadcastModeFor(resolution, count) {
  return `${resolution === "fhd" ? "fhd" : "720p"}_${count > 1 ? "multi" : "single"}`;
}

// modeAvailability는 고른 구성이 플랜에서 허용되는지와 안내 문구다. 사용량을 아직
// 못 읽었으면 막지 않는다 — 서버가 최종 판정한다(403).
function modeAvailability() {
  const platforms = selectedPlatforms();
  if (!platforms.length) {
    return { allowed: false, text: "플랫폼을 하나 이상 고르세요." };
  }
  const mode = broadcastModeFor(els.broadcastResolution.value, platforms.length);
  const label = MODE_LABELS[mode];
  const entry = state.usage?.available_by_mode?.find((item) => item.mode === mode);
  if (!entry) {
    return { allowed: true, text: `${label} 송출` };
  }
  if (!entry.allowed) {
    return { allowed: false, text: `${planLabel(state.usage.plan)} 플랜은 ${label} 송출을 쓸 수 없습니다.` };
  }
  const remaining = entry.seconds == null ? "무제한" : `${formatDuration(entry.seconds)} 가능`;
  return { allowed: true, text: `${label} · ${entry.multiplier}배 차감 · 이 방식으로 ${remaining}` };
}

function renderModeHint() {
  const { allowed, text } = modeAvailability();
  els.broadcastModeHint.textContent = text;
  els.broadcastModeHint.dataset.state = allowed ? "idle" : "error";
}

function planLabel(plan) {
  return plan ? plan.charAt(0).toUpperCase() + plan.slice(1) : "알 수 없음";
}

function formatDuration(seconds) {
  const minutes = Math.max(0, Math.floor(Number(seconds || 0) / 60));
  const hours = Math.floor(minutes / 60);
  return hours ? `${hours}시간 ${minutes % 60}분` : `${minutes}분`;
}

// afterSignIn은 로그인 직후 계정에 딸린 표시(플랜·연결 계정·카테고리)를 채운다.
async function afterSignIn() {
  await refreshStreamingAccounts().catch(() => null);
  await refreshPlan();
}

// refreshPlan은 요금제와 이번 달 방송 시간을 읽어 표시한다.
async function refreshPlan() {
  if (!state.accessToken) {
    return;
  }
  try {
    state.usage = await apiFetch("/users/me/usage");
  } catch (error) {
    state.usage = null;
    logEvent("warn", "Usage load failed", { message: error?.message });
  }
  renderPlan();
  renderModeHint();
  updateButtons();
}

function renderPlan() {
  const usage = state.usage;
  els.planSummary.hidden = !state.accessToken || !usage;
  if (!usage) {
    return;
  }
  els.planBadge.textContent = `${planLabel(usage.plan)} 플랜`;
  els.planUsage.textContent =
    usage.limit_seconds == null
      ? `이번 달 ${formatDuration(usage.used_seconds)} 방송 · 무제한`
      : `이번 달 남은 방송 시간 ${formatDuration(usage.remaining_seconds)} / ${formatDuration(usage.limit_seconds)}`;
}

// loadYoutubeCategories는 연결된 YouTube 계정으로 고를 수 있는 카테고리를 불러온다.
async function loadYoutubeCategories() {
  try {
    const payload = await apiFetch("/auth/youtube/categories");
    renderYoutubeCategories(payload?.categories || [], els.youtubeCategory.value);
  } catch (error) {
    logEvent("warn", "YouTube categories load failed", { message: error?.message });
  }
}

function renderYoutubeCategories(categories, selected) {
  els.youtubeCategory.replaceChildren();
  const none = document.createElement("option");
  none.value = "";
  none.textContent = "카테고리 없음";
  els.youtubeCategory.append(none);
  for (const category of categories) {
    const option = document.createElement("option");
    option.value = category.id;
    option.textContent = category.title;
    els.youtubeCategory.append(option);
  }
  state.youtubeCategoryIds = categories.map((category) => category.id);
  setYoutubeCategory(selected);
}

// setYoutubeCategory는 id를 고른다. 목록에 없는 id(목록 조회 전·지역 밖)는 항목을
// 더해 값이 사라지지 않게 한다.
function setYoutubeCategory(id) {
  if (id && !state.youtubeCategoryIds.includes(id)) {
    const option = document.createElement("option");
    option.value = id;
    option.textContent = `카테고리 ${id}`;
    els.youtubeCategory.append(option);
    state.youtubeCategoryIds.push(id);
  }
  els.youtubeCategory.value = id || "";
}


// renderTargets는 대상별 상태와 개별 제어를 그린다. 서버 응답의 targets[]는
// provider 이름 정렬이라 순서가 흔들리지 않는다.
function renderTargets(session) {
  // 방송하지 않는 대상(idle)은 빼고 그린다. 서버는 전환으로 뺀 대상도 idle로
  // 남기므로, 그대로 그리면 송출하지 않는 플랫폼이 목록에 남는다(#325).
  const targets = (session?.targets || []).filter(
    (target) => (target.stream?.broadcast_phase || "idle") !== "idle",
  );
  els.targetList.replaceChildren();
  els.targetListEmpty.hidden = targets.length > 0;
  for (const target of targets) {
    const row = document.createElement("li");
    row.className = "target-row";
    const name = document.createElement("span");
    name.className = "target-name";
    name.textContent = platformLabel(target.provider);
    const stateText = document.createElement("span");
    stateText.className = "target-state";
    stateText.textContent = describeTargetState(target.stream);
    row.append(name, stateText);
    for (const [action, label] of [
      ["pause", "일시 중지"],
      ["resume", "재개"],
      ["stop", "종료"],
    ]) {
      const button = document.createElement("button");
      button.className = action === "stop" ? "button button-danger" : "button";
      button.type = "button";
      button.textContent = label;
      button.addEventListener("click", () => void controlTarget(target.provider, action));
      row.append(button);
    }
    els.targetList.append(row);
  }
}

const TARGET_PHASE_LABELS = {
  idle: "대기",
  preparing: "준비 중",
  prepared: "준비됨",
  going_live: "라이브 전환 중",
  live: "라이브",
};
const TARGET_STREAM_LABELS = {
  idle: "연결 준비",
  streaming: "송출 중",
  reconnecting: "재연결 중",
  reconfiguring: "재구성 중",
  paused: "일시 중지",
  paused_reconfiguring: "일시 중지",
  paused_reconnecting: "일시 중지 · 재연결 중",
  stopped: "종료",
};

// describeTargetState는 대상 하나의 방송 단계와 송출 상태를 한 줄로 쓴다.
function describeTargetState(stream) {
  const phase = stream?.broadcast_phase || "idle";
  const status = stream?.status || "idle";
  const phaseText = TARGET_PHASE_LABELS[phase] || phase;
  return phase === "idle" ? phaseText : `${phaseText} · ${TARGET_STREAM_LABELS[status] || status}`;
}

// broadcastControlTargets는 상단 제어 버튼이 걸어야 할 대상이다. 준비를 거친
// 대상이 둘 이상일 때만 목록을 돌려준다 — 하나뿐이면 빈 배열이라 호출부가
// 종전처럼 provider 없이 부르고, 단독 송출 경로는 바뀌지 않는다.
function broadcastControlTargets() {
  const targets = (state.session?.targets || []).filter(
    (target) => (target.stream?.broadcast_phase || "idle") !== "idle",
  );
  return targets.length > 1 ? targets.map((target) => target.provider) : [];
}

// applyToEveryTarget은 제어를 대상 전부에 건다. 상단 버튼이 기본 대상만
// 끄면 동시 송출에서 나머지 플랫폼이 라이브로 남는데, 화면은 종료된 것처럼
// 보인다. 한 대상이 실패해도 나머지는 계속 건다 — 하나 때문에 다른 방송이
// 남는 것이 더 나쁘다.
async function applyToEveryTarget(action, providers, sessionId) {
  const failures = [];
  for (const provider of providers) {
    try {
      const stream = await apiFetch(`/sessions/${sessionId}/stream/${action}?provider=${provider}`, {
        method: "POST",
      });
      logEvent("ok", "Broadcast control applied", { session_id: sessionId, provider, action, stream });
    } catch (error) {
      const code = error?.payload?.error?.code || error?.message;
      failures.push(`${provider}: ${code}`);
      logEvent("error", "Broadcast control failed", { session_id: sessionId, provider, action, code });
    }
  }
  await refreshCurrentSession({ quiet: true });
  if (failures.length) {
    els.broadcastSettingsDetail.textContent = `일부 대상 제어 실패 — ${failures.join(" · ")}`;
  }
}

// controlTarget은 한 대상에만 제어를 건다. provider를 생략하면 서버가 세션의
// 기본 대상으로 보내므로, 개별 제어에는 반드시 실어야 한다.
async function controlTarget(provider, action) {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    return;
  }
  try {
    const stream = await apiFetch(`/sessions/${sessionId}/stream/${action}?provider=${provider}`, {
      method: "POST",
    });
    logEvent("ok", "Target control applied", { session_id: sessionId, provider, action, stream });
    await refreshCurrentSession({ quiet: true });
  } catch (error) {
    logEvent("error", "Target control failed", {
      session_id: sessionId,
      provider,
      action,
      code: error?.payload?.error?.code,
      message: error?.message,
    });
  }
}

// 치지직의 categoryId는 사용자가 알 수 없는 영문 식별자라 검색으로만 얻는다.
// 고른 결과의 종류·식별자는 항상 쌍으로 폼에 채운다.
async function searchChzzkCategories() {
  const query = els.chzzkCategoryQuery.value.trim();
  if (!query) {
    els.broadcastSettingsDetail.textContent = "카테고리 검색어를 입력하세요.";
    return;
  }
  try {
    // 치지직 응답에는 페이지네이션이 없어 상한(50)을 한 번에 받는다.
    const payload = await apiFetch(
      `/auth/chzzk/categories?query=${encodeURIComponent(query)}&size=50`,
    );
    state.chzzkCategories = payload.categories || [];
    renderChzzkCategoryResults();
    els.broadcastSettingsDetail.textContent = state.chzzkCategories.length
      ? `카테고리 ${state.chzzkCategories.length}건을 찾았습니다. 목록에서 고르세요.`
      : "검색 결과가 없습니다. 다른 이름으로 검색하세요.";
    logEvent("ok", "Chzzk categories searched", { query, count: state.chzzkCategories.length });
  } catch (error) {
    els.broadcastSettingsDetail.textContent = `카테고리 검색 실패: ${error.message}`;
    logEvent("error", "Chzzk category search failed", { message: error.message });
  }
}

function renderChzzkCategoryResults() {
  els.chzzkCategoryResults.replaceChildren();
  const placeholder = document.createElement("option");
  placeholder.value = "";
  placeholder.textContent = "검색 결과에서 고르세요";
  els.chzzkCategoryResults.append(placeholder);
  state.chzzkCategories.forEach((category, index) => {
    const option = document.createElement("option");
    // 값으로 색인을 쓴다 — 종류와 식별자를 쌍으로 되찾아야 하기 때문이다.
    option.value = String(index);
    option.textContent = `${category.category_value} (${category.category_type})`;
    els.chzzkCategoryResults.append(option);
  });
  els.chzzkCategoryResults.value = "";
}

function clearChzzkCategoryResults() {
  state.chzzkCategories = [];
  renderChzzkCategoryResults();
}

function applyChzzkCategorySelection() {
  // 안내 항목의 값은 빈 문자열이다. Number("")는 0이라 그대로 색인하면
  // 사용자가 고르지 않은 첫 결과가 적용된다.
  const index = els.chzzkCategoryResults.value;
  if (index === "") {
    return;
  }
  const selected = state.chzzkCategories[Number(index)];
  if (!selected) {
    return;
  }
  els.chzzkCategoryType.value = selected.category_type;
  els.chzzkCategoryId.value = selected.category_id;
  // 직접 고른 값이므로 기본값 주입이 덮지 않도록 표식을 남긴다.
  state.touchedBroadcastFields.add("chzzk:category_type");
  state.touchedBroadcastFields.add("chzzk:category_id");
  els.broadcastSettingsDetail.textContent = `카테고리를 ${selected.category_value}로 골랐습니다. 저장하세요.`;
}

async function createSessionOnly() {
  await runBusy(async () => {
    const session = await createSession();
    setCurrentSession(session);
    await refreshSessions({ quiet: true });
  });
}

async function createSession() {
  persistServerUrl();
  const metadata = buildSessionMetadata();
  const request = {
    method: "POST",
    // 첫 번째로 고른 플랫폼이 세션의 기본 대상이다. 해상도는 방송 전이면 준비
    // 직전에 다시 맞추고(#283), 방송 중이면 송출 방식 변경으로 바꾼다(#300).
    body: JSON.stringify({
      provider: selectedPlatforms()[0] || "youtube",
      broadcast_resolution: els.broadcastResolution.value,
      metadata,
    }),
  };
  let session;
  try {
    session = await apiFetch("/sessions", request);
  } catch (error) {
    // 새로고침 등으로 잃은 이전 세션이 남아 있으면 지우고 한 번만 다시 만든다.
    if (error.payload?.error?.code !== "session_already_exists" || !(await deleteRememberedSession())) {
      throw error;
    }
    session = await apiFetch("/sessions", request);
  }
  // owner_token은 여기서 정확히 한 번만 반환된다. 이후 세션 범위 요청과 signaling이
  // 소유권을 증명하도록 메모리에 보관하며, 세션 새로고침 응답에는 다시 오지 않는다.
  state.ownerToken = session.owner_token || null;
  rememberSession(session.session_id, state.ownerToken);
  state.platformDefaultsLoaded = new Set();
  logEvent("ok", "Session created", {
    session_id: session.session_id,
    broadcast_resolution: session.broadcast_resolution,
    metadata: session.metadata,
  });
  await applyBroadcastDefaults(session.session_id);
  return session;
}

// platformFormFields는 플랫폼 카드의 입력이다. 직전 방송 기본값 주입과 사용자가
// 건드린 필드 표시가 같은 목록을 본다.
function platformFormFields(provider) {
  return provider === "chzzk"
    ? {
        title: els.chzzkTitle,
        category_type: els.chzzkCategoryType,
        category_id: els.chzzkCategoryId,
        tags: els.chzzkTags,
      }
    : {
        title: els.youtubeTitle,
        description: els.youtubeDescription,
        category_id: els.youtubeCategory,
        privacy: els.youtubePrivacy,
        made_for_kids: els.youtubeMadeForKids,
      };
}

// applyBroadcastDefaults는 고른 플랫폼마다 직전 방송 값을 카드에 채운다(#143).
async function applyBroadcastDefaults(sessionId) {
  for (const provider of selectedPlatforms()) {
    await applyPlatformDefaults(sessionId, provider);
  }
}

// applyPlatformDefaults는 한 플랫폼의 직전 방송 값을 채운다. 사용자가 건드린
// 필드는 덮지 않고, 조회가 실패해도 폼은 그대로 쓴다.
async function applyPlatformDefaults(sessionId, provider) {
  // 끄면 카드를 비운 채로 둔다 — 카테고리 없이 보내려는 경우 등(#352).
  if (state.platformDefaultsLoaded.has(provider) || els.loadBroadcastDefaults?.checked === false) {
    return;
  }
  const untouched = (field) => !state.touchedBroadcastFields.has(`${provider}:${field}`);
  try {
    const defaults = await apiFetch(`/sessions/${sessionId}/broadcast/defaults?provider=${provider}`);
    state.platformDefaultsLoaded.add(provider);
    if (provider === "chzzk") {
      if (untouched("title")) {
        els.chzzkTitle.value = defaults.title || "";
      }
      if (untouched("category_id")) {
        els.chzzkCategoryId.value = defaults.category_id || "";
      }
      if (untouched("category_type")) {
        els.chzzkCategoryType.value = defaults.category_type || "";
      }
      if (untouched("tags")) {
        els.chzzkTags.value = (defaults.tags || []).join(",");
      }
    } else {
      if (untouched("title")) {
        els.youtubeTitle.value = defaults.title || "";
      }
      if (untouched("description")) {
        els.youtubeDescription.value = defaults.description || "";
      }
      if (untouched("category_id")) {
        setYoutubeCategory(defaults.category_id || "");
      }
      if (untouched("privacy") && defaults.privacy) {
        els.youtubePrivacy.value = defaults.privacy;
      }
      // 아동용 여부는 미선택(null)이면 사용자가 직접 골라야 하므로 두고 본다.
      if (untouched("made_for_kids") && typeof defaults.made_for_kids === "boolean") {
        els.youtubeMadeForKids.checked = defaults.made_for_kids;
      }
    }
    els.broadcastSettingsDetail.textContent = `${platformLabel(provider)} 직전 방송 값을 불러왔습니다. 확인 후 저장하세요.`;
    logEvent("ok", "Broadcast defaults loaded", { session_id: sessionId, provider, defaults });
  } catch (error) {
    logEvent("warn", "Broadcast defaults load failed", {
      session_id: sessionId,
      provider,
      message: error?.message,
      status: error?.status,
    });
  }
}

function buildSessionMetadata() {
  const label = els.sessionLabel.value.trim();
  const metadata = {
    client: "inno-live-web-client",
    client_id: getClientId(),
    started_at: new Date().toISOString(),
  };
  if (label) {
    metadata.label = label;
  }
  return metadata;
}

async function refreshSessions({ quiet = false } = {}) {
  // GET /sessions 목록 endpoint는 모든 활성 session_id를 노출해 서버에서
  // 제거했다. 이 viewer는 자신이 만든 세션만 소유하므로 panel에는 현재 세션만
  // 반영한다.
  const sessions = state.session ? [state.session] : [];
  renderSessions(sessions);
  if (!quiet) {
    logEvent("ok", "Sessions refreshed", { count: sessions.length });
  }
  return { sessions };
}

async function refreshCurrentSession({ quiet = true } = {}) {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    return null;
  }

  try {
    const session = await apiFetch(`/sessions/${sessionId}`);
    // 요청 중 사용자가 다른 세션으로 전환했으면, 늦게 도착한 이전 세션 응답이
    // 현재 세션 화면을 덮어쓰지 않게 버린다.
    if (state.session?.session_id !== sessionId) {
      return state.session;
    }
    setCurrentSession(session);
    if (!quiet) {
      logEvent("ok", "Session refreshed", { session_id: session.session_id });
    }
    return session;
  } catch (error) {
    if (error.status === 404) {
      logEvent("warn", "Current session no longer exists", {
        session_id: sessionId,
      });
      // recovery 만료 등으로 서버가 세션을 제거하면, polling 표시만 지우지
      // 않고 카메라·마이크·PeerConnection·signaling WebSocket까지 정리한다.
      // 요청 중 새 세션으로 바뀐 경우에는 그 새 연결을 정리하지 않는다.
      await cleanupConnection({
        keepSession: false,
        expectedSessionId: sessionId,
      });
      return null;
    }
    logError("Failed to refresh current session", error);
    throw error;
  }
}

function renderSessions(sessions) {
  els.sessionCount.textContent = String(sessions.length);
  els.sessionsList.replaceChildren();

  if (!sessions.length) {
    const empty = document.createElement("div");
    empty.className = "empty-state";
    empty.textContent = "No active sessions";
    els.sessionsList.append(empty);
    return;
  }

  for (const session of sessions) {
    const row = document.createElement("div");
    row.className = "session-row";

    const main = document.createElement("div");
    main.className = "session-main";
    const id = document.createElement("strong");
    id.textContent = session.session_id;
    const detail = document.createElement("span");
    detail.textContent = [
      session.status,
      session.peer_connection?.connection_state || "unknown",
      session.media?.raw_video_track ? "raw video" : "no video",
    ].join(" / ");
    main.append(id, detail);

    const actions = document.createElement("div");
    actions.className = "session-actions";
    const useButton = document.createElement("button");
    useButton.type = "button";
    useButton.textContent = "Use";
    useButton.addEventListener("click", () => {
      setCurrentSession(session);
      logEvent("ok", "Selected session", { session_id: session.session_id });
      updateButtons();
    });

    const deleteButton = document.createElement("button");
    deleteButton.type = "button";
    deleteButton.textContent = "Delete";
    deleteButton.addEventListener("click", () =>
      void deleteSessionById(session.session_id),
    );
    actions.append(useButton, deleteButton);
    row.append(main, actions);
    els.sessionsList.append(row);
  }
}

async function startWebRtc() {
  await runBusy(async () => {
    try {
      persistServerUrl();
      await healthCheck({ quiet: true });

      if (canStartYouTubeBroadcastFromCurrentConnection()) {
        logEvent("ok", "Reusing active WebRTC connection for broadcast", {
          session_id: state.session.session_id,
        });
        const readySession = await waitForVideoTrack();
        await prepareBroadcast(readySession);
        return;
      }

      // 방송 설정을 채우는 동안 미협상 세션이 회수될 수 있으므로(#147) offer 직전에
      // 서버에 남아 있는지 확인한다. 404면 refreshCurrentSession이 현재 세션을
      // 비우고, 아래에서 새 세션을 만들어 이어간다.
      if (state.session && !state.pc) {
        await refreshCurrentSession({ quiet: true });
      }

      const reusableSession = canUseCurrentSessionForOffer();
      if (!reusableSession) {
        await cleanupConnection({ keepSession: false });
      }

      await ensureLocalMedia();
      if (!reusableSession) {
        setCurrentSession(await createSession());
        // 회수된 세션과 함께 저장한 방송 설정도 사라진다. 사용자가 이미 저장했다면
        // 폼 값을 그대로 새 세션에 다시 보내 입력을 잃지 않게 한다.
        if (state.broadcastSettingsSaved) {
          await saveBroadcastSettings();
        }
      }
      setBroadcastStatus(els.broadcastVideoInput, "WebRTC 연결 중", "warn");
      setBroadcastStatus(els.broadcastRtmpState, "영상 입력 대기", "warn");
      setBroadcastStatus(els.broadcastPlatformState, "방송 준비 대기", "warn");
      await connectPeer(state.session.session_id);
      startPolling();
      await refreshCurrentSession({ quiet: true });
      await refreshSessions({ quiet: true });

      const readySession = await waitForVideoTrack();
      await prepareBroadcast(readySession);
    } catch (error) {
      await cleanupConnection({ keepSession: Boolean(state.session) });
      throw error;
    }
  });
}

function canStartYouTubeBroadcastFromCurrentConnection() {
  return Boolean(
    state.session?.session_id &&
      state.localStream &&
      state.pc?.connectionState === "connected",
  );
}

// 서버가 실제로 수신한 raw video track을 확인한 뒤에만 방송 시작을 요청한다.
// 시간만 기다리면 느린 카메라·네트워크에서 Prepare만 성공하고 송출 시작이
// 실패할 수 있으므로 iOS와 동일하게 세션 응답을 기준으로 판단한다.
async function waitForVideoTrack() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("방송을 시작할 세션이 없습니다.");
  }

  logEvent("ok", "Waiting for server video track", { session_id: sessionId });
  for (let attempt = 1; attempt <= VIDEO_TRACK_WAIT_ATTEMPTS; attempt += 1) {
    const connectionState = state.pc?.connectionState;
    if (["failed", "closed", "disconnected"].includes(connectionState)) {
      throw new Error("영상 트랙을 기다리는 중 WebRTC 연결이 끊겼습니다.");
    }

    const snapshot = await refreshCurrentSession({ quiet: true });
    if (!snapshot || snapshot.session_id !== sessionId) {
      throw new Error("영상 트랙을 기다리는 중 세션을 찾을 수 없습니다.");
    }
    if (snapshot.media?.raw_video_track?.ready_state === "live") {
      setBroadcastStatus(els.broadcastVideoInput, "서버 수신 확인됨", "ok");
      logEvent("ok", "Server video track detected", {
        session_id: sessionId,
        track: snapshot.media.raw_video_track,
      });
      return snapshot;
    }

    setBroadcastStatus(
      els.broadcastVideoInput,
      `서버 수신 대기 (${attempt}/${VIDEO_TRACK_WAIT_ATTEMPTS})`,
      "warn",
    );
    await delay(VIDEO_TRACK_WAIT_INTERVAL_MS);
  }

  throw new Error("서버가 영상 트랙을 받지 못했습니다. 카메라 연결을 다시 시도하세요.");
}

// readBroadcastThumbnail은 선택한 이미지를 base64로 바꾼다. PUT /broadcast가
// JSON 계약이라 바이너리를 그대로 실을 수 없다.
async function readBroadcastThumbnail() {
  const file = els.youtubeThumbnail.files?.[0];
  if (!file) {
    return null;
  }
  const bytes = new Uint8Array(await file.arrayBuffer());
  let binary = "";
  // btoa는 문자열만 받고 apply는 인자 수 제한이 있어 청크로 나눠 붙인다.
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return { mime: file.type, data_base64: btoa(binary) };
}

// platformSettingsPayload는 플랫폼 카드의 값을 PUT /broadcast 본문으로 옮긴다.
async function platformSettingsPayload(provider) {
  if (provider === "chzzk") {
    return {
      title: els.chzzkTitle.value.trim(),
      category_type: els.chzzkCategoryType.value,
      category_id: els.chzzkCategoryId.value.trim(),
      // 빈 태그는 제거하고 배열로 보낸다. 빈 문자열 하나를 보내면 서버가
      // tags[0] 비어 있음으로 거절한다.
      tags: els.chzzkTags.value
        .split(",")
        .map((tag) => tag.trim())
        .filter((tag) => tag !== ""),
    };
  }
  return {
    title: els.youtubeTitle.value.trim(),
    description: els.youtubeDescription.value,
    privacy: els.youtubePrivacy.value,
    made_for_kids: els.youtubeMadeForKids.checked,
    category_id: els.youtubeCategory.value,
    thumbnail: await readBroadcastThumbnail(),
  };
}

// liveSettingsPayload는 방송 중에 바꿀 수 있는 항목만 싣는다(#334). 공개 범위·
// 아동용 신고·썸네일은 서버가 거절한다.
async function liveSettingsPayload(provider) {
  const payload = await platformSettingsPayload(provider);
  if (provider === "chzzk") {
    return payload;
  }
  return { title: payload.title, description: payload.description, category_id: payload.category_id };
}

// applyLiveSettings는 송출을 끊지 않고 진행 중인 방송에 설정을 반영한다.
async function applyLiveSettings(provider) {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    return;
  }
  await runBusy(async () => {
    const body = await liveSettingsPayload(provider);
    const session = await apiFetch(`/sessions/${sessionId}/broadcast/live?provider=${provider}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    });
    setCurrentSession(session);
    setBroadcastStatus(els.broadcastSettingsState, "방송 중 적용됨", "ok");
    els.broadcastSettingsDetail.textContent = `${platformLabel(provider)} 방송에 설정을 반영했습니다(송출 유지).`;
    logEvent("ok", "Live broadcast settings applied", { session_id: sessionId, provider, request: body });
  });
}

// savePlatformSettings는 한 플랫폼의 방송 설정을 저장한다. 대상은 쿼리로 고른다 —
// 빼면 세션의 기본 대상에 덮어쓴다.
async function savePlatformSettings(sessionId, provider) {
  return apiFetch(`/sessions/${sessionId}/broadcast?provider=${provider}`, {
    method: "PUT",
    body: JSON.stringify(await platformSettingsPayload(provider)),
  });
}

// saveBroadcastSettings는 고른 플랫폼마다 방송 설정을 저장한다. PUT은 전체 교체라
// 비운 필드는 서버에서도 비워진다.
async function saveBroadcastSettings() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("방송 설정을 저장할 세션이 없습니다.");
  }
  const platforms = selectedPlatforms();
  if (!platforms.length) {
    throw new Error("송출할 플랫폼을 하나 이상 고르세요.");
  }
  let provider = platforms[0];
  try {
    let updated = null;
    for (provider of platforms) {
      updated = await savePlatformSettings(sessionId, provider);
    }
    setCurrentSession(updated);
    state.broadcastSettingsSaved = true;
    setBroadcastStatus(els.broadcastSettingsState, "저장됨", "ok");
    els.broadcastSettingsDetail.textContent = describeBroadcastSettings(updated, platforms);
    logEvent("ok", "Broadcast settings saved", { session_id: sessionId, platforms });
    return updated;
  } catch (error) {
    const details = error?.payload?.error?.details;
    setBroadcastStatus(els.broadcastSettingsState, "저장 실패", "error");
    els.broadcastSettingsDetail.textContent = `${platformLabel(provider)} — ${
      details?.field ? `${details.field}: ${details.reason || error.message}` : error.message
    }`;
    logEvent("error", "Broadcast settings save failed", {
      session_id: sessionId,
      provider,
      field: details?.field,
      message: error?.message,
      status: error?.status,
    });
    throw error;
  }
}

function describeBroadcastSettings(session, platforms) {
  return platforms
    .map((provider) => {
      if (provider === "chzzk") {
        const chzzk = session?.chzzk_broadcast;
        if (!chzzk) {
          return "치지직: 저장된 설정 없음";
        }
        return `치지직: ${[
          chzzk.title || "제목 없음",
          chzzk.category_type ? `${chzzk.category_type}/${chzzk.category_id || ""}` : "카테고리 없음",
          chzzk.tags?.length ? `태그 ${chzzk.tags.join(",")}` : "태그 없음",
        ].join(" · ")}`;
      }
      const broadcast = session?.broadcast;
      if (!broadcast) {
        return "YouTube: 저장된 설정 없음";
      }
      return `YouTube: ${[
        broadcast.title || "제목 없음",
        broadcast.privacy || "privacy 미설정",
        broadcast.category_id ? `카테고리 ${broadcast.category_id}` : "카테고리 없음",
        broadcast.thumbnail ? `썸네일 ${formatBytes(broadcast.thumbnail.bytes)}` : "썸네일 없음",
      ].join(" · ")}`;
    })
    .join(" / ");
}

// prepareBroadcast는 고른 플랫폼마다 방송을 준비한다. 한 플랫폼이 실패해도 나머지는
// 계속한다(서버의 동시 발사 규칙과 같다). 세션 해상도가 폼과 다르면 먼저 맞춘다 —
// 방송 전이라 해상도만 바뀐다(#283).
async function prepareBroadcast(session) {
  const sessionId = session?.session_id;
  if (!sessionId) {
    throw new Error("방송을 준비할 세션이 없습니다.");
  }
  const platforms = selectedPlatforms();
  setBroadcastStatus(els.broadcastRtmpState, "방송 준비 중", "warn");
  setBroadcastStatus(els.broadcastPlatformState, `${platforms.map(platformLabel).join("·")} 준비 중`, "warn");
  logEvent("ok", "Broadcast prepare requested", { session_id: sessionId, platforms });
  try {
    const resolution = els.broadcastResolution.value;
    if (session.broadcast_resolution && session.broadcast_resolution !== resolution) {
      setCurrentSession(
        await apiFetch(`/sessions/${sessionId}/broadcast-resolution`, {
          method: "PUT",
          body: JSON.stringify({ resolution }),
        }),
      );
    }
    // 준비 옵션은 저장된 설정이 단일 출처이므로 준비 직전에 폼을 반영한다.
    await saveBroadcastSettings();
  } catch (error) {
    renderBroadcastStartError(error);
    logEvent("error", "Broadcast prepare failed", { session_id: sessionId, message: error?.message });
    return;
  }
  const failures = [];
  for (const provider of platforms) {
    try {
      const prepared = await prepareTargetWithConfirm(sessionId, provider);
      setCurrentSession(prepared);
      renderBroadcastStreamStatus(prepared.stream);
      renderBroadcastWarnings(prepared.warnings);
      logEvent("ok", "Broadcast prepared", { session_id: sessionId, provider });
    } catch (error) {
      failures.push(`${platformLabel(provider)}: ${error?.payload?.error?.code || error?.message}`);
      logEvent("error", "Broadcast prepare failed", {
        session_id: sessionId,
        provider,
        code: error?.payload?.error?.code,
        message: error?.message,
        status: error?.status,
      });
      if (failures.length === platforms.length) {
        renderBroadcastStartError(error);
      }
    }
  }
  if (failures.length) {
    els.broadcastSettingsDetail.textContent = `준비 실패 — ${failures.join(" · ")}`;
  }
}

// prepareTargetWithConfirm은 대상 하나를 준비한다. 채널이 이미 다른 도구로 라이브면
// 서버가 409 channel_already_live로 묻는다 — 사용자가 동의하면 방송을 하나 더
// 연다(#361).
async function prepareTargetWithConfirm(sessionId, provider, confirmConcurrent = (message) => window.confirm(message)) {
  const request = (allowConcurrent) =>
    apiFetch(`/sessions/${sessionId}/stream/prepare`, {
      method: "POST",
      body: JSON.stringify(allowConcurrent ? { provider, allow_concurrent: true } : { provider }),
    });
  try {
    return await request(false);
  } catch (error) {
    if (error.payload?.error?.code !== "channel_already_live") {
      throw error;
    }
    const message =
      `${platformLabel(provider)} 채널이 이미 다른 도구로 라이브 중입니다.\n` +
      "계속하면 라이브가 하나 더 열려 시청자가 나뉠 수 있습니다. 계속할까요?";
    if (!confirmConcurrent(message)) {
      throw error;
    }
    return request(true);
  }
}

// goLiveBroadcast는 준비된 방송을 시청자에게 공개되는 라이브로 전환한다.
// 준비(prepare)와 분리되어 있어 화면 확인을 끝낸 뒤 누를 수 있다(#142).
async function goLiveBroadcast() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("라이브로 전환할 세션이 없습니다.");
  }
  await runBusy(async () => {
    setBroadcastStatus(els.broadcastPlatformState, "라이브 전환 중", "warn");
    try {
      const stream = await apiFetch(`/sessions/${sessionId}/stream/golive`, {
        method: "POST",
      });
      renderBroadcastStreamStatus(stream);
      setBroadcastStatus(els.broadcastPlatformState, "라이브", "ok");
      logEvent("ok", "Broadcast is live", { session_id: sessionId, stream });
      // 동시 송출에서 한쪽만 실패하면 요청은 200이고 사유만 실려 온다 —
      // 나머지 대상은 라이브로 남는다(#233). 조용히 지나가면 안 된다.
      if (stream?.failed_targets?.length) {
        const summary = stream.failed_targets
          .map((failure) => `${failure.provider}: ${failure.code}`)
          .join(" · ");
        setBroadcastStatus(els.broadcastPlatformState, "일부 대상 실패", "warn");
        els.broadcastSettingsDetail.textContent = `라이브 전환 실패한 대상 — ${summary}`;
        logEvent("warn", "Some simulcast targets failed to go live", {
          session_id: sessionId,
          failed_targets: stream.failed_targets,
        });
      }
      await refreshCurrentSession({ quiet: true });
    } catch (error) {
      renderBroadcastStartError(error);
      logEvent("error", "Go live failed", {
        session_id: sessionId,
        code: error?.payload?.error?.code,
        message: error?.message,
        status: error?.status,
      });
    }
  });
}

async function pauseBroadcast() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("일시 중지할 방송 세션이 없습니다.");
  }
  await runBusy(async () => {
    // 동시 송출에서는 상단 버튼도 대상 전부에 건다(#260).
    const providers = broadcastControlTargets();
    if (providers.length > 0) {
      await applyToEveryTarget("pause", providers, sessionId);
      return;
    }
    const paused = await apiFetch(`/sessions/${sessionId}/stream/pause`, {
      method: "POST",
    });
    updateCurrentSessionStream(paused);
    logEvent("ok", "Broadcast pause requested", {
      session_id: sessionId,
      stream: paused,
    });
  });
}

// changeBroadcastMode는 방송 중에 송출 구성의 해상도·플랫폼으로 송출 방식을 바꾼다
// (#300, 202). 해상도가 바뀌면 서버가 방송을 끝내고 새 구성으로 다시 연다. 진행은
// 세션 응답의 resolution_switch로 폴링된다. 새로 더하는 플랫폼은 카드의 설정을
// 먼저 저장한다.
async function changeBroadcastMode() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("송출 방식을 바꿀 세션이 없습니다.");
  }
  await runBusy(async () => {
    const liveTargets = liveTargetProviders();
    const targets = selectedPlatforms();
    for (const provider of targets) {
      if (!liveTargets.includes(provider)) {
        await savePlatformSettings(sessionId, provider);
      }
    }
    const body = { resolution: els.broadcastResolution.value, targets };
    const session = await apiFetch(`/sessions/${sessionId}/broadcast-mode`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
    setCurrentSession(session);
    logEvent("ok", "Broadcast mode change requested", {
      session_id: sessionId,
      request: body,
      resolution_switch: session.resolution_switch,
    });
  });
}

const UPGRADE_MODE_LABELS = {
  fhd_single: "FHD로 올리기",
  "720p_multi": "720p 동시 송출",
  fhd_multi: "FHD 동시 송출",
};

// renderUpgradeOffer는 빈자리로 송출 방식을 올릴 수 있다는 서버 제안을 선택지마다
// 버튼으로 보인다(#278, #333). 하나를 수락하면 서버가 제안 전체를 지운다.
function renderUpgradeOffer(session) {
  const offer = session?.upgrade_offer;
  els.upgradeOffer.hidden = !offer;
  if (!offer) {
    els.upgradeOfferOptions.replaceChildren();
    return;
  }
  // 선택지를 고른 뒤에는 새 플랫폼 설정을 채우고 확정하는 단계다 — 다른 선택지는 숨긴다.
  els.confirmUpgradeBtn.hidden = !offer.selected;
  if (offer.selected) {
    els.upgradeOfferText.textContent =
      `${UPGRADE_MODE_LABELS[offer.selected] || offer.selected}: 새 플랫폼의 방송 설정을 채운 뒤 '설정 완료하고 전환'을 누르세요(3분 안).`;
    els.upgradeOfferOptions.replaceChildren();
    return;
  }
  els.upgradeOfferText.textContent = "빈자리가 생겨 송출 방식을 올릴 수 있어요.";
  const buttons = (offer.options || []).map((option) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "button button-primary";
    const remaining = option.remaining_seconds_after == null ? "" : `, 약 ${formatDuration(option.remaining_seconds_after)}`;
    button.textContent = `${UPGRADE_MODE_LABELS[option.mode] || option.mode} (${offer.units_from}배 → ${option.units_to}배${remaining})`;
    button.addEventListener("click", () => void acceptUpgradeOffer(option));
    return button;
  });
  els.upgradeOfferOptions.replaceChildren(...buttons);
}

// upgradeRestartNotice는 재시작 선택지를 수락하기 전에 반드시 보일 안내다(#333).
function upgradeRestartNotice(option) {
  const effects = (option.restart_effects || []).map((effect) =>
    effect.same_link
      ? `- ${platformLabel(effect.provider)}: 같은 주소에서 약 ${effect.gap_seconds}초 뒤 새 방송으로 시작(시청자는 새로고침 없이 이어서 봄)`
      : `- ${platformLabel(effect.provider)}: 새 방송 링크, 지금 시청자는 끊김`,
  );
  return ["방송이 종료되고 새 방송으로 다시 시작됩니다.", ...effects].join("\n");
}

// acceptUpgradeOffer는 선택지 하나를 수락한다. 재시작 선택지는 안내에 동의해야만
// 진행한다. 플랫폼을 더하는 선택지는 보류를 늘리고 새 플랫폼 설정 카드를 연다 —
// 설정을 채운 뒤 '송출 방식 변경'으로 확정한다. 해상도만 바꾸는 선택지는 바로 전환한다.
async function acceptUpgradeOffer(option, confirmRestart = (message) => window.confirm(message)) {
  const sessionId = state.session?.session_id;
  if (!sessionId || !option) {
    return;
  }
  if (option.restarts_broadcast && !confirmRestart(upgradeRestartNotice(option))) {
    return;
  }
  await runBusy(async () => {
    // 코드로 바꾼 값은 change 이벤트를 내지 않으므로 캡처도 직접 맞춘다(#331).
    els.broadcastResolution.value = option.resolution;
    await syncCaptureResolution();
    if (option.needs_settings) {
      const session = await apiFetch(`/sessions/${sessionId}/upgrade-offer/select`, {
        method: "POST",
        body: JSON.stringify({ mode: option.mode }),
      });
      for (const provider of PLATFORMS) {
        platformToggle(provider).checked = option.targets.includes(provider);
      }
      applyPlatformSelection();
      // 코드로 켠 카드는 change 이벤트가 없으므로 직전 방송 값을 직접 채운다.
      const liveTargets = liveTargetProviders();
      for (const provider of option.targets.filter((target) => !liveTargets.includes(target))) {
        await applyPlatformDefaults(sessionId, provider);
      }
      setCurrentSession(session);
      logEvent("ok", "Upgrade option selected", { session_id: sessionId, mode: option.mode });
      return;
    }
    const body = { resolution: option.resolution, targets: option.targets };
    const session = await apiFetch(`/sessions/${sessionId}/broadcast-mode`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
    setCurrentSession(session);
    logEvent("ok", "Upgrade offer accepted", { session_id: sessionId, request: body });
  });
}

// confirmUpgradeOption은 고른 선택지를 확정한다: 새 플랫폼 설정을 저장한 뒤 선택지의
// 구성으로 전환한다. 재시작 안내는 선택할 때 이미 동의받았다.
async function confirmUpgradeOption() {
  const sessionId = state.session?.session_id;
  const offer = state.session?.upgrade_offer;
  const option = offer?.options?.find((candidate) => candidate.mode === offer.selected);
  if (!sessionId || !option) {
    return;
  }
  await runBusy(async () => {
    const liveTargets = liveTargetProviders();
    for (const provider of option.targets) {
      if (!liveTargets.includes(provider)) {
        await savePlatformSettings(sessionId, provider);
      }
    }
    const body = { resolution: option.resolution, targets: option.targets };
    const session = await apiFetch(`/sessions/${sessionId}/broadcast-mode`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
    setCurrentSession(session);
    logEvent("ok", "Upgrade offer accepted", { session_id: sessionId, request: body });
  });
}

async function declineUpgradeOffer() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    return;
  }
  await runBusy(async () => {
    const session = await apiFetch(`/sessions/${sessionId}/upgrade-offer`, { method: "DELETE" });
    setCurrentSession(session);
    logEvent("ok", "Upgrade offer declined", { session_id: sessionId });
  });
}

function liveTargetProviders() {
  return (state.session?.targets || [])
    .filter((target) => target.stream?.broadcast_phase === "live")
    .map((target) => target.provider);
}

// renderSwitchStatus는 송출 방식 전환의 진행을 송출 구성 영역에 알린다. 같은 전환의
// 같은 상태는 한 번만 알린다 — 폴링마다 다른 메시지를 덮으면 안 된다.
function renderSwitchStatus(session) {
  const change = session?.resolution_switch;
  if (!change) {
    return;
  }
  const key = `${change.started_at}:${change.status}`;
  if (key === state.lastSwitchNotice) {
    return;
  }
  state.lastSwitchNotice = key;
  const failed = (change.failed_targets || [])
    .map((failure) => `${platformLabel(failure.provider)}: ${failure.code}`)
    .join(" · ");
  const target = `${(change.targets || []).map(platformLabel).join("·")} ${change.resolution === "fhd" ? "FHD" : "720p"}`;
  const views = {
    switching: ["전환 중", "warn", `${target}로 바꾸는 중입니다. 해상도가 바뀌면 새 방송으로 다시 열립니다.`],
    done: [failed ? "일부 실패" : "전환 완료", failed ? "warn" : "ok", failed ? `${target} — 실패: ${failed}` : `${target}로 바꿨습니다.`],
    failed: ["전환 실패", "error", `${target} — ${failed || "새 방송을 열지 못했습니다."}`],
    canceled: ["전환 취소", "warn", "방송 종료로 전환을 멈췄습니다."],
  };
  const [label, visual, detail] = views[change.status] || [change.status, "warn", ""];
  setBroadcastStatus(els.broadcastSettingsState, label, visual);
  els.broadcastSettingsDetail.textContent = detail;
  if (change.status !== "switching") {
    void refreshPlan();
  }
}

// stopBroadcast는 YouTube 송출만 끝낸다. 세션과 WebRTC 미리보기는 그대로
// 남으므로, 설정을 고쳐 곧바로 다음 방송을 준비할 수 있다.
async function stopBroadcast() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("종료할 방송 세션이 없습니다.");
  }
  await runBusy(async () => {
    // 동시 송출에서는 상단 버튼도 대상 전부에 건다(#260).
    const providers = broadcastControlTargets();
    if (providers.length > 0) {
      await applyToEveryTarget("stop", providers, sessionId);
    } else {
      const stopped = await apiFetch(`/sessions/${sessionId}/stream/stop`, {
        method: "POST",
      });
      updateCurrentSessionStream(stopped);
      logEvent("ok", "Broadcast stop requested", {
        session_id: sessionId,
        stream: stopped,
      });
    }
    // 끝난 방송만큼 이번 달 남은 시간이 줄었다.
    await refreshPlan();
  });
}

async function resumeBroadcast() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("재개할 방송 세션이 없습니다.");
  }
  await runBusy(async () => {
    // 동시 송출에서는 상단 버튼도 대상 전부에 건다(#260).
    const providers = broadcastControlTargets();
    if (providers.length > 0) {
      await applyToEveryTarget("resume", providers, sessionId);
      return;
    }
    const resumed = await apiFetch(`/sessions/${sessionId}/stream/resume`, {
      method: "POST",
    });
    updateCurrentSessionStream(resumed);
    logEvent("ok", "Broadcast resume requested", {
      session_id: sessionId,
      stream: resumed,
    });
  });
}

// renderBroadcastWarnings는 카테고리·썸네일처럼 실패해도 방송이 진행되는
// 선택 항목의 경고를 보여준다(#141).
function renderBroadcastWarnings(warnings) {
  if (!warnings?.length) {
    return;
  }
  setBroadcastStatus(els.broadcastSettingsState, "일부 미반영", "warn");
  els.broadcastSettingsDetail.textContent = warnings
    .map((warning) => NOTICE_MESSAGES[warning.code] || warning.message)
    .join(" / ");
  logEvent("warn", "Broadcast settings partially applied", { warnings });
}

function renderBroadcastStartError(error) {
  const code = error?.payload?.error?.code;
  if (code === "streaming_not_connected") {
    setBroadcastStatus(els.broadcastAccounts, "연결 필요", "warn");
    setBroadcastStatus(els.broadcastRtmpState, "시작 안 함");
    setBroadcastStatus(els.broadcastPlatformState, "플랫폼 계정 연결 필요", "warn");
    return;
  }
  if (code === "streaming_reconnect_required") {
    setBroadcastStatus(els.broadcastAccounts, "재연결 필요", "warn");
    setBroadcastStatus(els.broadcastRtmpState, "시작 안 함");
    setBroadcastStatus(els.broadcastPlatformState, "플랫폼 계정 재연결 필요", "warn");
    return;
  }
  if (code === "streaming_rate_limited") {
    setBroadcastStatus(els.broadcastRtmpState, "시작 안 함");
    setBroadcastStatus(els.broadcastPlatformState, "플랫폼 요청 한도 초과 · 잠시 후 다시", "error");
    return;
  }
  if (code === "live_streaming_blocked") {
    setBroadcastStatus(els.broadcastRtmpState, "시작 안 함");
    setBroadcastStatus(els.broadcastPlatformState, "채널 라이브 권한 없음", "error");
    return;
  }
  if (code === "broadcast_not_ready") {
    // 송출 프레임이 플랫폼에 아직 도착하지 않은 상태 — 잠시 후 재시도.
    setBroadcastStatus(els.broadcastPlatformState, "라이브 전환 대기 · 잠시 후 재시도", "warn");
    return;
  }
  if (code === "broadcast_prepared" || code === "broadcast_live") {
    setBroadcastStatus(els.broadcastPlatformState, "이미 준비된 방송이 있음", "warn");
    return;
  }
  if (code === "broadcast_going_live") {
    setBroadcastStatus(els.broadcastPlatformState, "라이브 전환 중", "warn");
    return;
  }
  if (code === "broadcast_stopped") {
    // 전환 왕복 중에 중지가 들어와 중지가 이긴 경우다(#142).
    setBroadcastStatus(els.broadcastPlatformState, "중지되어 라이브 취소됨", "warn");
    return;
  }
  if (code === "stream_already_active") {
    setBroadcastStatus(els.broadcastRtmpState, "이미 송출 중", "warn");
    setBroadcastStatus(els.broadcastPlatformState, "기존 방송 사용 중 · live 확인 전", "warn");
    return;
  }
  setBroadcastStatus(els.broadcastRtmpState, "시작 실패", "error");
  setBroadcastStatus(els.broadcastPlatformState, "방송 준비 실패", "error");
}

function delay(milliseconds) {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

function canUseCurrentSessionForOffer() {
  if (!state.session || state.pc) {
    return false;
  }
  return (
    state.session.status === "active" &&
    state.session.timing?.session_to_offer_ms == null &&
    state.session.peer_connection?.signaling_state === "stable"
  );
}

async function ensureLocalMedia() {
  if (location.protocol === "file:") {
    throw new Error(
      "카메라는 file:// 페이지에서 사용할 수 없습니다. 서버의 /client/ 주소를 HTTP(S)로 여세요.",
    );
  }
  if (!navigator.mediaDevices?.getUserMedia) {
    throw new Error(
      "이 주소에서는 카메라 API를 사용할 수 없습니다. HTTPS 또는 localhost로 접속하세요.",
    );
  }

  stopLocalStream();
  const constraints = {
    video: buildVideoConstraints(),
    audio: els.sendAudio.checked,
  };
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia(constraints);
  } catch (error) {
    if (
      error?.name === "NotAllowedError" ||
      /permission denied/i.test(error?.message || "")
    ) {
      throw new Error(
        "카메라 권한이 거부되었습니다. 브라우저의 카메라 권한을 허용한 뒤 다시 시도하세요.",
      );
    }
    throw error;
  }
  state.localStream = stream;
  els.localVideo.srcObject = stream;
  updateLocalTrackState();
  logEvent("ok", "Local media opened", describeStream(stream));
  await loadCameras();
}

// captureResolutionFor는 송출 해상도에 맞는 캡처 해상도다(#322). FHD로 송출하는데
// 720p로 찍으면 서버가 업스케일해 화질이 오르지 않는다.
function captureResolutionFor(broadcastResolution) {
  return broadcastResolution === "fhd" ? "fhd" : "hd";
}

// syncCaptureResolution은 캡처 선택을 송출 해상도에 맞추고, 카메라가 열려 있으면
// 트랙에 바로 적용한다. applyConstraints라 재협상 없이 바뀐다.
async function syncCaptureResolution() {
  const capture = captureResolutionFor(els.broadcastResolution.value);
  if (els.resolutionSelect.value === capture) {
    return;
  }
  els.resolutionSelect.value = capture;
  const [videoTrack] = state.localStream?.getVideoTracks() || [];
  if (!videoTrack) {
    return;
  }
  const { deviceId, ...constraints } = buildVideoConstraints();
  try {
    await videoTrack.applyConstraints(constraints);
    logEvent("ok", "Capture resolution changed", videoTrack.getSettings?.() || constraints);
  } catch (error) {
    logEvent("warn", "Capture resolution change failed", error?.message || String(error));
  }
}

function buildVideoConstraints() {
  const selectedCamera = els.cameraSelect.value;
  const resolution = els.resolutionSelect.value;
  const video = {};

  if (selectedCamera) {
    video.deviceId = { exact: selectedCamera };
  }
  if (resolution === "fhd") {
    video.width = { ideal: 1920 };
    video.height = { ideal: 1080 };
  }
  if (resolution === "hd") {
    video.width = { ideal: 1280 };
    video.height = { ideal: 720 };
  }
  if (resolution === "sd") {
    video.width = { ideal: 640 };
    video.height = { ideal: 360 };
  }
  return video;
}

async function loadCameras() {
  if (!navigator.mediaDevices?.enumerateDevices) {
    return;
  }
  const selected = els.cameraSelect.value;
  try {
    const devices = await navigator.mediaDevices.enumerateDevices();
    const cameras = devices.filter((device) => device.kind === "videoinput");
    els.cameraSelect.replaceChildren();
    const defaultOption = document.createElement("option");
    defaultOption.value = "";
    defaultOption.textContent = "Default camera";
    els.cameraSelect.append(defaultOption);

    cameras.forEach((camera, index) => {
      const option = document.createElement("option");
      option.value = camera.deviceId;
      option.textContent = camera.label || `Camera ${index + 1}`;
      els.cameraSelect.append(option);
    });
    els.cameraSelect.value = selected;
  } catch (error) {
    logError("Failed to enumerate cameras", error);
  }
}

async function connectPeer(sessionId) {
  resetRemoteStream();
  state.candidateQueue = [];
  state.remoteCandidateQueue = [];
  state.offerSent = false;
  state.activeNegotiationId = null;
  state.localCandidateNegotiationIds.clear();
  state.remoteDescriptionNegotiationId = null;
  state.lastSelectedCandidatePairId = null;

  const config = await loadWebRTCConfig({ allowFallback: true });
  const pc = new RTCPeerConnection({ iceServers: config.iceServers });
  state.pc = pc;
  wirePeerConnection(pc);

  addLocalTracks(pc);

  await negotiatePeer(pc, sessionId, { iceRestart: false });
}

// negotiatePeer는 첫 연결과 네트워크 복구 ICE restart가 공유하는 offer·answer
// 경로다. negotiation ID는 뒤늦게 도착한 이전 후보를 서버와 클라이언트 모두에서
// 버리기 위한 세대 식별자다.
async function negotiatePeer(pc, sessionId, { iceRestart }) {
  state.candidateQueue = [];
  state.remoteCandidateQueue = [];
  state.offerSent = false;
  state.localCandidateNegotiationIds.clear();
  const negotiationId = createNegotiationId();
  state.activeNegotiationId = negotiationId;
  // 새 offer를 시작한 순간부터 이전 remoteDescription은 이 세대의 후보를 받을
  // 수 없다. 새 answer의 setRemoteDescription이 끝날 때까지 후보를 queue한다.
  state.remoteDescriptionNegotiationId = null;

  await ensureSignalingSocket();

  try {
    if (iceRestart) {
      pc.restartIce();
    }

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    rememberLocalCandidateGeneration(pc.localDescription, negotiationId);
    updatePeerUi();

    sendSignaling({
      type: "offer",
      session_id: sessionId,
      owner_token: state.ownerToken,
      access_token: state.accessToken,
      sdp: pc.localDescription.sdp,
      negotiation_id: negotiationId,
      ice_restart: iceRestart,
    });
    state.offerSent = true;
    flushCandidateQueue();

    // WebSocket message는 현재 JavaScript 작업이 끝난 뒤에 처리되므로 offer를
    // 보낸 직후 여기서 waiter를 등록해도 answer를 놓치지 않는다. 반대로 offer
    // 생성·전송 실패 전에 waiter를 만들면 timeout rejection이 고아가 된다.
    const answer = await waitForAnswer(
      sessionId,
      negotiationId,
      negotiationTimeoutMs(iceRestart),
    );
    await pc.setRemoteDescription({
      type: "answer",
      sdp: answer.sdp,
    });
    state.remoteDescriptionNegotiationId = negotiationId;
    await flushRemoteCandidateQueue(negotiationId);
    updatePeerUi();
    logEvent("ok", "Remote answer applied", {
      session_id: answer.session_id,
      negotiation_id: negotiationId,
      ice_restart: iceRestart,
      sdp_lines: answer.sdp.split(/\r?\n/).length,
    });
  } catch (error) {
    // answer를 적용하지 못한 local offer는 PeerConnection을 have-local-offer에
    // 남긴다. 이를 rollback하지 않으면 이후 복구 시도가 stable 상태를 기다리다
    // 복구 창을 모두 소진하게 된다.
    if (iceRestart) {
      await rollbackRecoveryOffer(pc, sessionId, negotiationId);
    }
    throw error;
  }
}

function negotiationTimeoutMs(iceRestart) {
  if (!iceRestart || !state.recovery.active) {
    return 15000;
  }
  const remainingMs = state.recovery.deadlineAt - Date.now() - RECOVERY_CONNECTION_RESERVE_MS;
  return Math.max(1000, Math.min(RECOVERY_ANSWER_TIMEOUT_MS, remainingMs));
}

async function rollbackRecoveryOffer(pc, sessionId, negotiationId) {
  if (
    state.pc !== pc ||
    state.activeNegotiationId !== negotiationId ||
    pc.signalingState !== "have-local-offer"
  ) {
    return;
  }
  try {
    await pc.setLocalDescription({ type: "rollback" });
    state.offerSent = false;
    state.candidateQueue = [];
    state.remoteCandidateQueue = [];
    state.localCandidateNegotiationIds.clear();
    state.remoteDescriptionNegotiationId = null;
    updatePeerUi();
    logEvent("warn", "Rolled back incomplete ICE restart offer", {
      session_id: sessionId,
      negotiation_id: negotiationId,
    });
  } catch (error) {
    logError("Failed to roll back incomplete ICE restart offer", error);
  }
}

function createNegotiationId() {
  if (crypto.randomUUID) {
    return crypto.randomUUID();
  }
  return `00000000-0000-4000-8000-${Date.now().toString(16).padStart(12, "0").slice(-12)}`;
}

async function ensureSignalingSocket() {
  if (state.ws?.readyState === WebSocket.OPEN) {
    return state.ws;
  }
  return openSignalingSocket();
}

async function loadWebRTCConfig({ allowFallback }) {
  try {
    const payload = await apiFetch("/webrtc/config");
    const iceServers = Array.isArray(payload?.iceServers)
      ? payload.iceServers
      : [];
    const recovery = normalizeRecoveryPolicy(payload?.recovery);
    state.webrtcConfig = { iceServers, recovery };
    if (!iceServers.length) {
      logEvent("warn", "Server returned no ICE servers; using host candidates only");
      return state.webrtcConfig;
    }
    logEvent("ok", "Loaded ICE server configuration", {
      urls: iceServers.map((server) => server.urls),
      recovery,
    });
    return state.webrtcConfig;
  } catch (error) {
    if (!allowFallback) {
      throw error;
    }
    // 처음 연결할 때만 보수적인 STUN 기본값을 쓴다. 복구 중 이 값으로 기존
    // TURN 설정을 덮으면 VPN·망 전환에서 relay 후보를 잃을 수 있다.
    logError("Failed to load ICE server configuration; using default STUN", error);
    state.webrtcConfig = {
      iceServers: DEFAULT_ICE_SERVERS,
      recovery: DEFAULT_RECOVERY_POLICY,
    };
    return state.webrtcConfig;
  }
}

function normalizeRecoveryPolicy(value) {
  const windowMs = Number(value?.window_ms);
  const debounceMs = Number(value?.debounce_ms);
  const maxAttempts = Number(value?.max_attempts);
  return {
    window_ms: Number.isFinite(windowMs) && windowMs > 0 ? windowMs : DEFAULT_RECOVERY_POLICY.window_ms,
    debounce_ms: Number.isFinite(debounceMs) && debounceMs >= 0 ? debounceMs : DEFAULT_RECOVERY_POLICY.debounce_ms,
    max_attempts: Number.isInteger(maxAttempts) && maxAttempts > 0 ? maxAttempts : DEFAULT_RECOVERY_POLICY.max_attempts,
  };
}

function scheduleNetworkRecovery(pc, reason, { immediate = false } = {}) {
  if (
    state.closingConnection ||
    state.pc !== pc ||
    !state.session?.session_id ||
    pc.connectionState === "closed"
  ) {
    return;
  }

  if (!state.recovery.active) {
    state.recovery.active = true;
    state.recovery.attempts = 0;
    state.recovery.generation += 1;
    state.recovery.deadlineAt = Date.now() + state.webrtcConfig.recovery.window_ms;
    state.recovery.initialOfferPending = false;
    state.recovery.reason = reason;
    logEvent("warn", "WebRTC network recovery scheduled", {
      session_id: state.session.session_id,
      reason,
      recovery_deadline: new Date(state.recovery.deadlineAt).toISOString(),
      server_max_ice_restart_offers: state.webrtcConfig.recovery.max_attempts,
    });
    startNetworkRecoveryStatusObserver(pc, state.recovery.generation, reason);
  }

  // recovery 창마다 ICE restart offer는 한 번만 전송한다. 이후 failed 이벤트는
  // 브라우저가 같은 ICE 검사 실패를 다시 알린 것일 수 있으므로, 기존 검사를
  // 취소하거나 새 offer를 만들지 않고 상태 관찰만 계속한다.
  if (state.recovery.attempts > 0) {
    updatePeerUi();
    return;
  }

  if (state.recovery.debounceTimer) {
    if (!immediate) {
      updatePeerUi();
      return;
    }
    window.clearTimeout(state.recovery.debounceTimer);
    state.recovery.debounceTimer = null;
  }

  const generation = state.recovery.generation;
  const start = () => {
    state.recovery.debounceTimer = null;
    void runNetworkRecoveryAttempt(pc, generation, reason);
  };
  if (immediate) {
    start();
  } else {
    state.recovery.debounceTimer = window.setTimeout(
      start,
      state.webrtcConfig.recovery.debounce_ms,
    );
  }
  updatePeerUi();
}

async function runNetworkRecoveryAttempt(pc, generation, reason) {
  if (!isCurrentRecovery(pc, generation)) {
    return;
  }
  if (pc.connectionState === "connected") {
    completeNetworkRecovery(pc, generation);
    return;
  }
  if (Date.now() >= state.recovery.deadlineAt) {
    logEvent("error", "WebRTC recovery deadline reached; server will close the session", {
      session_id: state.session?.session_id,
      attempts: state.recovery.attempts,
      reason,
    });
    updatePeerUi();
    return;
  }
  // 이 테스트 클라이언트의 정상 경로는 recovery 창당 하나의 ICE restart다.
  // 서버의 max_attempts는 반복 offer를 보내는 클라이언트에 대한 방어 한도이며,
  // 이 로컬 상태 확인 주기와는 관계가 없다.
  if (state.recovery.attempts > 0) {
    return;
  }
  if (pc.signalingState !== "stable") {
    state.recovery.initialOfferPending = true;
    logEvent("warn", "Waiting for signaling state before initial ICE restart", {
      session_id: state.session?.session_id,
      signaling_state: pc.signalingState,
      reason,
    });
    return;
  }

  state.recovery.initialOfferPending = false;
  state.recovery.attempts += 1;
  updatePeerUi();
  try {
    if (!isCurrentRecovery(pc, generation)) {
      return;
    }
    // 최초 연결에서 생성한 PeerConnection은 이미 /webrtc/config의 ICE/TURN
    // 설정을 가진다. 복구 중 재조회가 실패해 restart offer 자체가 막히지 않게
    // 기존 구성을 그대로 재사용한다.
    await ensureSignalingSocket();
    await negotiatePeer(pc, state.session.session_id, { iceRestart: true });
    logEvent("ok", "ICE restart offer accepted", {
      session_id: state.session.session_id,
      attempt: state.recovery.attempts,
      reason,
    });
  } catch (error) {
    logError("ICE restart attempt failed", error);
    if (error?.status === 401 || error?.code === "unauthorized") {
      logEvent("error", "WebRTC recovery stopped because authentication could not be refreshed", {
        session_id: state.session?.session_id,
      });
      await cleanupConnection({ keepSession: false });
      return;
    }
  }
}

function startNetworkRecoveryStatusObserver(pc, generation, reason) {
  if (!isCurrentRecovery(pc, generation) || state.recovery.statusTimer) {
    return;
  }
  const observe = () => {
    if (!isCurrentRecovery(pc, generation)) {
      return;
    }
    if (pc.connectionState === "connected") {
      completeNetworkRecovery(pc, generation);
      return;
    }

    const remainingMs = state.recovery.deadlineAt - Date.now();
    if (remainingMs <= 0) {
      state.recovery.statusTimer = null;
      logEvent("error", "WebRTC recovery deadline reached; server will close the session", {
        session_id: state.session?.session_id,
        attempts: state.recovery.attempts,
        reason,
      });
      updatePeerUi();
      return;
    }

    logEvent("warn", "WebRTC recovery status observed", {
      session_id: state.session?.session_id,
      connection_state: pc.connectionState,
      ice_connection_state: pc.iceConnectionState,
      ice_gathering_state: pc.iceGatheringState,
      signaling_state: pc.signalingState,
      ice_restart_offers: state.recovery.attempts,
      remaining_ms: remainingMs,
    });

    state.recovery.statusTimer = window.setTimeout(
      observe,
      Math.min(RECOVERY_STATUS_CHECK_INTERVAL_MS, remainingMs),
    );
    updatePeerUi();
  };
  state.recovery.statusTimer = window.setTimeout(
    observe,
    RECOVERY_STATUS_CHECK_INTERVAL_MS,
  );
}

function isCurrentRecovery(pc, generation) {
  return (
    !state.closingConnection &&
    state.pc === pc &&
    state.recovery.active &&
    state.recovery.generation === generation
  );
}

function completeNetworkRecovery(pc, generation) {
  if (!isCurrentRecovery(pc, generation)) {
    return;
  }
  const attempts = state.recovery.attempts;
  clearNetworkRecovery();
  logEvent("ok", "WebRTC peer connection recovered", {
    session_id: state.session?.session_id,
    attempts,
  });
  updatePeerUi();
}

function clearNetworkRecovery() {
  window.clearTimeout(state.recovery.debounceTimer);
  window.clearTimeout(state.recovery.statusTimer);
  state.recovery.active = false;
  state.recovery.attempts = 0;
  state.recovery.deadlineAt = 0;
  state.recovery.debounceTimer = null;
  state.recovery.statusTimer = null;
  state.recovery.initialOfferPending = false;
  state.recovery.reason = null;
}

function wirePeerConnection(pc) {
  pc.addEventListener("icecandidate", (event) => {
    logLocalCandidate(event.candidate);
    queueOrSendCandidate(event.candidate);
  });
  pc.addEventListener("track", (event) => {
    addRemoteTrack(event.track);
  });
  pc.addEventListener("connectionstatechange", () => {
    updatePeerUi();
    logEvent("ok", "Peer connection state changed", {
      connectionState: pc.connectionState,
    });
    if (pc.connectionState === "connected") {
      void logSelectedCandidatePair(pc, "connectionstatechange");
      completeNetworkRecovery(pc, state.recovery.generation);
    } else if (pc.connectionState === "disconnected") {
      scheduleNetworkRecovery(pc, "peer_connection_disconnected");
    } else if (pc.connectionState === "failed") {
      scheduleNetworkRecovery(pc, "peer_connection_failed", { immediate: true });
    }
  });
  pc.addEventListener("iceconnectionstatechange", () => {
    updatePeerUi();
    logEvent("ok", "ICE connection state changed", {
      iceConnectionState: pc.iceConnectionState,
    });
    if (["connected", "completed"].includes(pc.iceConnectionState)) {
      void logSelectedCandidatePair(pc, "iceconnectionstatechange");
    }
  });
  pc.addEventListener("signalingstatechange", () => {
    updatePeerUi();
    logEvent("ok", "Signaling state changed", {
      signalingState: pc.signalingState,
    });
    // debounce 시점에 기존 협상이 끝나지 않아 최초 restart offer를 보내지 못한
    // 경우만 여기서 한 번 시작한다. 5초 상태 관찰 타이머는 offer를 만들지 않는다.
    if (
      state.recovery.active &&
      state.recovery.initialOfferPending &&
      state.recovery.attempts === 0 &&
      pc.signalingState === "stable"
    ) {
      state.recovery.initialOfferPending = false;
      void runNetworkRecoveryAttempt(
        pc,
        state.recovery.generation,
        state.recovery.reason,
      );
    }
  });
  pc.addEventListener("icegatheringstatechange", () => {
    updatePeerUi();
    logEvent("ok", "ICE gathering state changed", {
      iceGatheringState: pc.iceGatheringState,
    });
  });
}

function logLocalCandidate(candidate) {
  if (!candidate) {
    logEvent("ok", "Local ICE gathering completed");
    return;
  }
  logEvent("ok", "Generated local ICE candidate", describeIceCandidate(candidate));
}

function addLocalTracks(pc) {
  const [videoTrack] = state.localStream.getVideoTracks();
  if (!videoTrack) {
    throw new Error("No local video track is available.");
  }

  pc.addTransceiver(videoTrack, {
    direction: "sendrecv",
    streams: [state.localStream],
  });

  for (const audioTrack of state.localStream.getAudioTracks()) {
    pc.addTrack(audioTrack, state.localStream);
  }

  logEvent("ok", "Local tracks added to peer connection", {
    video_direction: "sendrecv",
    audio_tracks: state.localStream.getAudioTracks().length,
  });
}

function addRemoteTrack(track) {
  if (!state.remoteStream.getTracks().some((item) => item.id === track.id)) {
    state.remoteStream.addTrack(track);
  }
  els.remoteVideo.srcObject = state.remoteStream;
  void els.remoteVideo.play().catch((error) => {
    logError("Remote video playback did not start automatically", error);
  });
  track.addEventListener("ended", updateRemoteTrackState);
  updateRemoteTrackState();
  logEvent("ok", "Remote track received", {
    id: track.id,
    kind: track.kind,
    readyState: track.readyState,
  });
}

function openSignalingSocket() {
  return new Promise((resolve, reject) => {
    const url = signalingUrl();
    const ws = new WebSocket(url);
    let opened = false;
    state.ws = ws;
    setPill(els.websocketState, "WS connecting", "warn");
    updateButtons();

    const timeout = window.setTimeout(() => {
      reject(new Error("Timed out while opening signaling WebSocket."));
      try {
        ws.close();
      } catch {
        return;
      }
    }, 10000);

    ws.addEventListener("open", () => {
      window.clearTimeout(timeout);
      opened = true;
      setPill(els.websocketState, "WS open", "ok");
      logEvent("ok", "Signaling WebSocket opened", { url });
      updateButtons();
      resolve(ws);
    });

    ws.addEventListener("message", (event) => {
      void handleSignalingMessage(event.data);
    });

    ws.addEventListener("error", () => {
      window.clearTimeout(timeout);
      setPill(els.websocketState, "WS error", "error");
      if (!opened) {
        reject(new Error("Failed to open signaling WebSocket."));
      }
      updateButtons();
    });

    ws.addEventListener("close", (event) => {
      window.clearTimeout(timeout);
      if (state.ws === ws) {
        state.ws = null;
      }
      setPill(els.websocketState, "WS closed", event.wasClean ? "idle" : "warn");
      logEvent(event.wasClean ? "warn" : "error", "Signaling WebSocket closed", {
        code: event.code,
        reason: event.reason || "none",
      });
      if (state.answerWaiter) {
        rejectWaitingAnswer(new Error("Signaling WebSocket closed before answer."));
      }
      updateButtons();
    });
  });
}

function waitForAnswer(sessionId, negotiationId, timeoutMs) {
  if (state.answerWaiter) {
    rejectWaitingAnswer(new Error("Replaced pending answer waiter."));
  }

  return new Promise((resolve, reject) => {
    const timeout = window.setTimeout(() => {
      const error = new Error("Timed out waiting for WebRTC answer.");
      error.code = "webrtc_answer_timeout";
      rejectWaitingAnswer(error);
    }, timeoutMs);

    state.answerWaiter = {
      sessionId,
      negotiationId,
      resolve: (payload) => {
        window.clearTimeout(timeout);
        state.answerWaiter = null;
        resolve(payload);
      },
      reject: (error) => {
        window.clearTimeout(timeout);
        state.answerWaiter = null;
        reject(error);
      },
    };
  });
}

function rejectWaitingAnswer(error) {
  if (!state.answerWaiter) {
    return;
  }
  state.answerWaiter.reject(error);
}

async function handleSignalingMessage(rawData) {
  const payload = parseJsonOrText(rawData);
  if (!payload || typeof payload !== "object") {
    logEvent("warn", "Received non-JSON signaling message", { rawData });
    return;
  }

  if (payload.type === "answer") {
    const isWaitingForAnswer = Boolean(
      state.answerWaiter &&
      state.answerWaiter.sessionId === payload.session_id &&
      state.answerWaiter.negotiationId === payload.negotiation_id,
    );
    if (isWaitingForAnswer) {
      logEvent("ok", "Received WebRTC answer", payload);
      state.answerWaiter.resolve(payload);
    } else {
      // timeout 뒤 도착한 answer는 rollback된 local offer의 응답일 수 있다.
      // 이를 현재 answer처럼 적용하면 새 ICE restart 세대와 충돌하므로 버린다.
      logEvent("warn", "Dropped late or stale WebRTC answer", {
        session_id: payload.session_id,
        negotiation_id: payload.negotiation_id,
        active_negotiation_id: state.activeNegotiationId,
      });
    }
    return;
  }

  if (payload.type === "ice_candidate_added") {
    logEvent("ok", "Server accepted ICE candidate", payload);
    return;
  }

  if (payload.type === "ice_candidate") {
    await addRemoteCandidate(payload);
    return;
  }

  if (payload.type === "error") {
    logEvent("error", "Signaling error response", payload);
    if (state.answerWaiter) {
      const error = new Error(payload.error?.message || "Signaling error response.");
      error.code = payload.error?.code;
      rejectWaitingAnswer(error);
    }
    return;
  }

  logEvent("warn", "Received unknown signaling message", payload);
}

async function addRemoteCandidate(payload) {
  const negotiationId = payload.negotiation_id;
  if (!negotiationId || negotiationId !== state.activeNegotiationId) {
    logEvent("warn", "Dropped stale remote ICE candidate", {
      negotiation_id: negotiationId || null,
      active_negotiation_id: state.activeNegotiationId,
    });
    return;
  }
  const candidateInit = payload.candidate
    ? {
        candidate: payload.candidate,
        sdpMid: payload.sdpMid ?? payload.sdp_mid ?? null,
        sdpMLineIndex: payload.sdpMLineIndex ?? payload.sdp_mline_index ?? null,
      }
    : null;

  logEvent("ok", "Received remote ICE candidate", {
    session_id: payload.session_id,
    end_of_candidates: candidateInit === null,
    ...describeCandidateString(payload.candidate),
  });

  if (!state.pc) {
    logEvent("warn", "Dropped remote ICE candidate because peer connection is absent");
    return;
  }

  if (
    !state.pc.remoteDescription ||
    state.remoteDescriptionNegotiationId !== negotiationId
  ) {
    state.remoteCandidateQueue.push({ negotiationId, candidateInit });
    logEvent("warn", "Queued remote ICE candidate until current answer is applied", {
      negotiation_id: negotiationId,
      remote_description_negotiation_id: state.remoteDescriptionNegotiationId,
      queued: state.remoteCandidateQueue.length,
    });
    return;
  }

  await state.pc.addIceCandidate(candidateInit);
  logEvent("ok", "Added remote ICE candidate", {
    end_of_candidates: candidateInit === null,
    ...describeCandidateString(payload.candidate),
  });
}

async function flushRemoteCandidateQueue(negotiationId) {
  while (state.remoteCandidateQueue.length) {
    const queued = state.remoteCandidateQueue.shift();
    if (queued.negotiationId !== negotiationId) {
      logEvent("warn", "Dropped queued stale remote ICE candidate", {
        negotiation_id: queued.negotiationId,
        active_negotiation_id: negotiationId,
      });
      continue;
    }
    const { candidateInit } = queued;
    await state.pc.addIceCandidate(candidateInit);
    logEvent("ok", "Added queued remote ICE candidate", {
      end_of_candidates: candidateInit === null,
      ...describeCandidateString(candidateInit?.candidate),
    });
  }
}

function queueOrSendCandidate(candidate) {
  // end-of-candidates 표시는 원격에 별도로 전달하지 않는다. 새 generation으로
  // 잘못 표기되면 느린 TURN candidate 수집을 끝난 것으로 처리할 수 있고, 이
  // 프로토콜에서는 answer 및 이후 trickle candidate만으로 충분하다.
  if (!candidate) {
    logEvent("ok", "Local ICE gathering completed");
    return;
  }

  const usernameFragment = localCandidateUsernameFragment(candidate);
  const negotiationId = usernameFragment
    ? state.localCandidateNegotiationIds.get(usernameFragment)
    : null;
  if (!negotiationId || negotiationId !== state.activeNegotiationId) {
    // 새 offer를 시작한 직후 이전 ICE generation에서 나온 candidate는 현재
    // negotiation ID로 다시 표기하지 않고 버린다. usernameFragment를 알 수 없는
    // candidate도 안전하게 연결할 세대를 판단할 수 없으므로 보내지 않는다.
    logEvent("warn", "Dropped local ICE candidate from unknown or stale generation", {
      candidate_username_fragment: usernameFragment,
      active_negotiation_id: state.activeNegotiationId,
    });
    return;
  }

  const payload = {
    type: "ice_candidate",
    session_id: state.session?.session_id,
    owner_token: state.ownerToken,
    access_token: state.accessToken,
    negotiation_id: negotiationId,
    candidate: candidate.candidate,
    sdpMid: candidate.sdpMid,
    sdpMLineIndex: candidate.sdpMLineIndex,
  };

  if (!state.offerSent) {
    state.candidateQueue.push(payload);
    return;
  }
  sendSignaling(payload);
}

function flushCandidateQueue() {
  while (state.candidateQueue.length) {
    sendSignaling(state.candidateQueue.shift());
  }
}

// rememberLocalCandidateGeneration은 setLocalDescription이 확정한 SDP에서 모든
// ICE username fragment를 찾아 현재 negotiation ID에 연결한다. candidate event의
// usernameFragment가 이 목록에 없으면 이전 generation 또는 판별 불가 후보다.
function rememberLocalCandidateGeneration(description, negotiationId) {
  state.localCandidateNegotiationIds.clear();
  const usernameFragments = new Set(
    [...(description?.sdp || "").matchAll(/^a=ice-ufrag:([^\r\n]+)$/gm)].map(
      (match) => match[1].trim(),
    ),
  );
  for (const usernameFragment of usernameFragments) {
    if (usernameFragment) {
      state.localCandidateNegotiationIds.set(usernameFragment, negotiationId);
    }
  }

  if (!state.localCandidateNegotiationIds.size) {
    logEvent("warn", "Local offer did not contain an ICE username fragment", {
      negotiation_id: negotiationId,
    });
  }
}

// localCandidateUsernameFragment는 표준 usernameFragment 속성을 우선 사용한다.
// 일부 브라우저가 이 속성을 제공하지 않는 경우 candidate SDP의 ufrag 확장값을
// 읽는다. 둘 다 없으면 세대를 안전하게 판단할 수 없다.
function localCandidateUsernameFragment(candidate) {
  if (typeof candidate?.usernameFragment === "string" && candidate.usernameFragment) {
    return candidate.usernameFragment;
  }
  const match = candidate?.candidate?.match(/(?:^|\s)ufrag\s+([^\s]+)/);
  return match?.[1] || null;
}

function sendSignaling(payload) {
  if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
    throw new Error("Signaling WebSocket is not open.");
  }
  state.ws.send(JSON.stringify(payload));
  logEvent("ok", "Sent signaling message", payload);
}

function sendErrorProbe() {
  try {
    sendSignaling({
      type: "unsupported_probe",
      sent_at: new Date().toISOString(),
    });
  } catch (error) {
    logError("Failed to send error probe", error);
  }
}

async function disconnect() {
  await runBusy(async () => {
    await cleanupConnection({ keepSession: true });
    await refreshCurrentSession({ quiet: true }).catch(() => null);
    await refreshSessions({ quiet: true }).catch(() => null);
  });
}

async function deleteCurrentSession() {
  if (!state.session?.session_id) {
    return;
  }
  await deleteSessionById(state.session.session_id);
}

async function deleteSessionById(sessionId) {
  await runBusy(async () => {
    const isCurrent = state.session?.session_id === sessionId;
    if (isCurrent) {
      await cleanupConnection({ keepSession: true });
    }
    try {
      await apiFetch(`/sessions/${sessionId}`, { method: "DELETE" });
      logEvent("ok", "Session deleted", { session_id: sessionId });
    } catch (error) {
      if (error.status === 404) {
        logEvent("warn", "Session was already gone", { session_id: sessionId });
      } else {
        throw error;
      }
    }

    if (isCurrent) {
      clearCurrentSession();
    }
    await refreshSessions({ quiet: true });
  });
}

async function cleanupConnection({ keepSession, expectedSessionId = null }) {
  // polling 중이던 이전 세션의 404가 새로 생성한 세션의 미디어 자원을 닫지 않게
  // 한다. expectedSessionId가 없으면 사용자 버튼에서 명시적으로 요청한 정리다.
  if (
    expectedSessionId &&
    state.session?.session_id !== expectedSessionId
  ) {
    return false;
  }

  state.closingConnection = true;
  clearNetworkRecovery();
  stopPolling();
  rejectWaitingAnswer(new Error("Connection cleanup started."));

  if (state.ws) {
    try {
      state.ws.close(1000, "client disconnect");
    } catch {
      logEvent("warn", "Failed to close WebSocket cleanly");
    } finally {
      state.ws = null;
    }
  }

  if (state.pc) {
    try {
      state.pc.close();
    } catch {
      logEvent("warn", "Failed to close peer connection cleanly");
    } finally {
      state.pc = null;
    }
  }

  stopLocalStream();
  resetRemoteStream();
  state.candidateQueue = [];
  state.remoteCandidateQueue = [];
  state.offerSent = false;
  state.activeNegotiationId = null;
  state.localCandidateNegotiationIds.clear();
  state.remoteDescriptionNegotiationId = null;
  state.lastSelectedCandidatePairId = null;
  setPill(els.websocketState, "WS idle", "idle");
  updatePeerUi();

  if (!keepSession) {
    clearCurrentSession();
  }
  state.closingConnection = false;
  updateButtons();
  return true;
}

function stopLocalStream() {
  if (!state.localStream) {
    updateLocalTrackState();
    return;
  }
  for (const track of state.localStream.getTracks()) {
    track.stop();
  }
  state.localStream = null;
  els.localVideo.srcObject = null;
  updateLocalTrackState();
}

function resetRemoteStream() {
  for (const track of state.remoteStream.getTracks()) {
    state.remoteStream.removeTrack(track);
  }
  els.remoteVideo.srcObject = state.remoteStream;
  updateRemoteTrackState();
}

function startPolling() {
  stopPolling();
  if (!els.autoPoll.checked || !state.session?.session_id) {
    return;
  }
  state.pollTimer = window.setInterval(() => {
    void refreshCurrentSession({ quiet: true });
  }, 2000);
}

function stopPolling() {
  if (state.pollTimer) {
    window.clearInterval(state.pollTimer);
    state.pollTimer = null;
  }
}

function setCurrentSession(session) {
  state.session = session;
  state.lastSessionJson = session;
  renderSessionDetails(session);
  renderTargets(session);
  renderSwitchStatus(session);
  renderUpgradeOffer(session);
  renderSessionNotices(session);
  updateButtons();
}

// NOTICE_MESSAGES는 사용자에게 보일 세션 알림 문구다. 한도 알림처럼 목록에 없는
// 코드는 기존 표시(방송 상태)에 맡긴다.
const NOTICE_MESSAGES = {
  youtube_quota_low:
    "오늘 유튜브 API 사용량이 많아 송출 방식 변경이나 새 방송이 실패할 수 있어요. 한국 시간 오후 4시(겨울 5시)에 초기화됩니다.",
  platform_broadcast_ended: "플랫폼 스튜디오에서 방송이 종료되어 송출을 멈췄어요.",
  channel_live_elsewhere: "치지직 채널이 이미 다른 도구로 방송 중이라 송출이 거절됐어요. 다른 도구의 방송을 먼저 끝내세요.",
};

// renderSessionNotices는 새로 생긴 세션 알림을 사용자가 닫을 때까지 남는 배너로
// 보인다(#366, #369). 상태 한 줄은 다른 메시지가 곧 덮어써 놓치기 쉽다. 같은
// 알림은 한 번만 띄운다.
function renderSessionNotices(session) {
  for (const notice of session?.notices || []) {
    const message = NOTICE_MESSAGES[notice.code];
    const key = `${session.session_id}:${notice.code}`;
    if (!message || state.shownNotices.has(key)) {
      continue;
    }
    state.shownNotices.add(key);
    logEvent("warn", message, { code: notice.code });
    const banner = document.createElement("div");
    banner.className = "upgrade-offer";
    const text = document.createElement("span");
    text.textContent = message;
    const close = document.createElement("button");
    close.type = "button";
    close.className = "button";
    close.textContent = "닫기";
    close.addEventListener("click", () => banner.remove());
    banner.append(text, close);
    els.sessionNotices.append(banner);
  }
}

// updateCurrentSessionStream은 pause·resume API가 반환한 stream 상태만 현재
// 세션에 반영한다. 이 API들은 전체 SessionResponse가 아니라 StreamState를
// 반환하므로 setCurrentSession에 직접 넘기면 session_id가 사라진다.
function updateCurrentSessionStream(stream) {
  if (!state.session?.session_id) {
    throw new Error("현재 세션 없이 방송 상태를 갱신할 수 없습니다.");
  }
  setCurrentSession({ ...state.session, stream });
}

function clearCurrentSession() {
  state.session = null;
  state.ownerToken = null;
  forgetSession();
  state.lastSessionJson = null;
  renderSessionDetails(null);
  updateButtons();
}

function renderSessionDetails(session) {
  els.copyJsonBtn.disabled = !session;
  els.sessionJson.textContent = JSON.stringify(session || {}, null, 2);

  if (!session) {
    els.sessionId.textContent = "none";
    els.sessionStatus.textContent = "idle";
    els.connectionState.textContent = "idle";
    els.iceState.textContent = "idle";
    els.signalingState.textContent = "idle";
    els.aiFallback.textContent = "false";
    els.videoSenderActive.textContent = "false";
    els.ignoredTracks.textContent = "0";
    setTiming({});
    renderBroadcastStreamStatus(null);
    return;
  }

  els.sessionId.textContent = session.session_id;
  els.sessionStatus.textContent = session.status;
  els.connectionState.textContent =
    session.peer_connection?.connection_state || "unknown";
  els.iceState.textContent =
    session.peer_connection?.ice_connection_state || "unknown";
  els.signalingState.textContent =
    session.peer_connection?.signaling_state || "unknown";
  els.aiFallback.textContent = String(
    session.media?.ai_fallback_active || false,
  );
  els.videoSenderActive.textContent = String(
    session.media?.video_sender_active || false,
  );
  els.ignoredTracks.textContent = String(
    session.media?.ignored_track_count || 0,
  );
  setTiming(session.timing || {});
  renderBroadcastStreamStatus(session.stream);
}

function setTiming(timing) {
  els.sessionToOffer.textContent = formatMs(timing.session_to_offer_ms);
  els.offerToAnswer.textContent = formatMs(timing.offer_to_answer_ms);
  els.offerToConnected.textContent = formatMs(timing.offer_to_connected_ms);
  els.answerToConnected.textContent = formatMs(timing.answer_to_connected_ms);
  els.offerToIceDone.textContent = formatMs(timing.offer_to_ice_completed_ms);
  els.answerToIceDone.textContent = formatMs(
    timing.answer_to_ice_completed_ms,
  );
}

function formatMs(value) {
  return value == null ? "-" : String(value);
}

function updatePeerUi() {
  const pc = state.pc;
  if (!pc) {
    setPill(els.peerState, "Peer idle", "idle");
    return;
  }
  const connection = pc.connectionState || "unknown";
  const ice = pc.iceConnectionState || "unknown";
  const signaling = pc.signalingState || "unknown";

  if (state.recovery.active) {
    const remainingSeconds = Math.max(
      0,
      Math.ceil((state.recovery.deadlineAt - Date.now()) / 1000),
    );
    const restartDetail = state.recovery.attempts > 0
      ? `ICE restart ${state.recovery.attempts}회`
      : "ICE restart 대기";
    setPill(
      els.peerState,
      `Peer recovering · ${restartDetail} · ${remainingSeconds}s`,
      "warn",
    );
    updateButtons();
    return;
  }
  const visualState = getVisualState(connection);
  setPill(els.peerState, `Peer ${connection}`, visualState);

  if (!state.session) {
    els.connectionState.textContent = connection;
    els.iceState.textContent = ice;
    els.signalingState.textContent = signaling;
  }
  updateButtons();
}

function updateLocalTrackState() {
  els.localTrackState.textContent = state.localStream
    ? describeStream(state.localStream).summary
    : "no track";
}

function updateRemoteTrackState() {
  els.remoteTrackState.textContent = state.remoteStream.getTracks().length
    ? describeStream(state.remoteStream).summary
    : "waiting";
}

function describeStream(stream) {
  const tracks = stream.getTracks();
  const video = tracks.filter((track) => track.kind === "video").length;
  const audio = tracks.filter((track) => track.kind === "audio").length;
  return {
    summary: `${video} video / ${audio} audio`,
    tracks: tracks.map((track) => ({
      id: track.id,
      kind: track.kind,
      readyState: track.readyState,
    })),
  };
}

function describeIceCandidate(candidate) {
  if (!candidate) {
    return { end_of_candidates: true };
  }
  const parsed = describeCandidateString(candidate.candidate);
  return {
    candidate: candidate.candidate,
    type: candidate.type || parsed.type,
    ip: candidate.address || candidate.ip || parsed.ip,
    port: candidate.port || parsed.port,
    protocol: candidate.protocol || parsed.protocol,
    sdpMid: candidate.sdpMid,
    sdpMLineIndex: candidate.sdpMLineIndex,
  };
}

function describeCandidateString(candidate) {
  if (!candidate || typeof candidate !== "string") {
    return {
      candidate: candidate || null,
      type: null,
      ip: null,
      port: null,
      protocol: null,
    };
  }

  const bits = candidate.replace(/^candidate:/, "").trim().split(/\s+/);
  const typeIndex = bits.indexOf("typ");
  return {
    candidate,
    type: typeIndex >= 0 ? bits[typeIndex + 1] || null : null,
    ip: bits[4] || null,
    port: bits[5] ? Number(bits[5]) : null,
    protocol: bits[2] || null,
  };
}

async function logSelectedCandidatePair(pc, reason) {
  try {
    const stats = await pc.getStats();
    const pair = findSelectedCandidatePair(stats);
    if (!pair) {
      logEvent("warn", "Selected ICE candidate pair is not available yet", {
        reason,
      });
      return;
    }
    if (state.lastSelectedCandidatePairId === pair.id) {
      return;
    }
    state.lastSelectedCandidatePairId = pair.id;

    const local = stats.get(pair.localCandidateId);
    const remote = stats.get(pair.remoteCandidateId);
    logEvent("ok", "Selected ICE candidate pair", {
      reason,
      pair: {
        id: pair.id,
        state: pair.state,
        nominated: pair.nominated,
        currentRoundTripTime: pair.currentRoundTripTime,
      },
      local: describeStatsCandidate(local),
      remote: describeStatsCandidate(remote),
    });
  } catch (error) {
    logError("Failed to inspect selected ICE candidate pair", error);
  }
}

function findSelectedCandidatePair(stats) {
  for (const report of stats.values()) {
    if (report.type !== "transport" || !report.selectedCandidatePairId) {
      continue;
    }
    const pair = stats.get(report.selectedCandidatePairId);
    if (pair) {
      return pair;
    }
  }

  for (const report of stats.values()) {
    if (
      report.type === "candidate-pair" &&
      (report.selected || (report.nominated && report.state === "succeeded"))
    ) {
      return report;
    }
  }
  return null;
}

function describeStatsCandidate(candidate) {
  if (!candidate) {
    return null;
  }
  return {
    id: candidate.id,
    type: candidate.candidateType,
    protocol: candidate.protocol,
    ip: candidate.address || candidate.ip,
    port: candidate.port,
    relayProtocol: candidate.relayProtocol,
    url: candidate.url,
  };
}

function getVisualState(value) {
  if (["connected", "completed"].includes(value)) {
    return "ok";
  }
  if (["connecting", "checking", "new"].includes(value)) {
    return "warn";
  }
  if (["failed", "closed", "disconnected"].includes(value)) {
    return "error";
  }
  return "idle";
}

function setPill(element, text, visualState) {
  element.textContent = text;
  element.dataset.state = visualState;
}

function updateButtons() {
  const hasSession = Boolean(state.session?.session_id);
  const hasConnection = Boolean(state.pc || state.ws || state.localStream);
  const wsOpen = state.ws?.readyState === WebSocket.OPEN;
  const fileProtocol = location.protocol === "file:";
  const signedIn = Boolean(state.accessToken);
  const streamStatus = state.session?.stream?.status;

  els.signInBtn.disabled = state.busy;
  els.signUpBtn.disabled = state.busy;
  els.verifyBtn.disabled = state.busy;
  els.signOutBtn.disabled = state.busy;
  // 세션 API는 RequireUser 뒤에 있으므로 로그인한 사용자에게만 열어 둔다.
  const mode = modeAvailability();
  const switching = state.session?.resolution_switch?.status === "switching";
  els.startBtn.disabled = state.busy || fileProtocol || !signedIn || !mode.allowed;
  // 라이브 전환은 준비된 방송에만 열어 둔다 — 서버도 409로 막지만 버튼이
  // 흐름(준비 → 라이브)을 그대로 보여줘야 한다.
  els.goLiveBtn.disabled =
    state.busy ||
    !state.session?.session_id ||
    state.session?.stream?.broadcast_phase !== "prepared";
  els.pauseBroadcastBtn.disabled =
    state.busy ||
    !state.session?.session_id ||
    !["streaming", "reconfiguring"].includes(streamStatus);
  els.resumeBroadcastBtn.disabled =
    state.busy ||
    !state.session?.session_id ||
    streamStatus !== "paused";
  // 송출 방식 변경은 라이브 중에만 연다. 방송 전 해상도·플랫폼은 "방송 준비"가
  // 송출 구성을 그대로 쓴다.
  els.changeResolutionBtn.disabled =
    state.busy || !mode.allowed || switching || liveTargetProviders().length === 0;
  // 방송 중 설정 변경은 그 플랫폼 방송이 준비됐거나 라이브일 때만 받는다(#334).
  const onAir = (provider) =>
    (state.session?.targets || []).some(
      (target) => target.provider === provider && ["prepared", "live"].includes(target.stream?.broadcast_phase),
    );
  els.youtubeApplyLiveBtn.disabled = state.busy || switching || !onAir("youtube");
  els.chzzkApplyLiveBtn.disabled = state.busy || switching || !onAir("chzzk");
  // 종료는 egress가 살아 있는 모든 상태에서 열어 둔다 — 재연결·일시 중지
  // 중에도 방송을 끝낼 수 있어야 한다. 서버가 ErrStreamNotActive로 보는
  // idle·stopped만 막는다.
  els.stopBroadcastBtn.disabled =
    state.busy ||
    !state.session?.session_id ||
    !streamStatus ||
    ["idle", "stopped"].includes(streamStatus);
  els.healthBtn.disabled = state.busy;
  els.createSessionBtn.disabled = state.busy || !signedIn;
  els.refreshSessionsBtn.disabled = state.busy;
  els.disconnectBtn.disabled = state.busy || !hasConnection;
  els.deleteSessionBtn.disabled = state.busy || !hasSession;
  // 방송 설정은 세션 범위 API라 세션이 있어야 저장할 수 있다.
  els.saveBroadcastBtn.disabled = state.busy || !hasSession;
  els.errorProbeBtn.disabled = state.busy || !wsOpen;
  els.copyJsonBtn.disabled = !state.lastSessionJson;
  els.referenceFaceInput.disabled =
    state.referenceFaceBusy || state.referenceFaceSupported === false;
  els.uploadReferenceFaceBtn.disabled =
    state.referenceFaceBusy ||
    state.referenceFaceSupported === false ||
    !els.referenceFaceInput.files.length;
  els.refreshReferenceFaceBtn.disabled =
    state.referenceFaceBusy || state.referenceFaceSupported === false;
  els.deleteReferenceFaceBtn.disabled =
    state.referenceFaceBusy ||
    !state.referenceFace?.registered ||
    state.referenceFace?.source !== "api";
}

async function runBusy(work) {
  if (state.busy) {
    return;
  }
  state.busy = true;
  updateButtons();
  try {
    await work();
  } catch (error) {
    logError("Operation failed", error);
  } finally {
    state.busy = false;
    updateButtons();
  }
}

async function copySessionJson() {
  if (!state.lastSessionJson || !navigator.clipboard) {
    return;
  }
  try {
    await navigator.clipboard.writeText(
      JSON.stringify(state.lastSessionJson, null, 2),
    );
    logEvent("ok", "Session JSON copied");
  } catch (error) {
    logError("Failed to copy session JSON", error);
  }
}

function logError(message, error) {
  logEvent("error", message, {
    message: error?.message || String(error),
    status: error?.status,
    payload: error?.payload,
  });
}

function logEvent(kind, message, payload) {
  const item = document.createElement("li");
  item.dataset.kind = kind;

  const time = document.createElement("time");
  time.dateTime = new Date().toISOString();
  time.textContent = new Date().toLocaleTimeString();
  item.append(time);

  const body = document.createElement("div");
  body.textContent = message;
  item.append(body);

  if (payload !== undefined) {
    const details = document.createElement("pre");
    details.className = "event-details";
    details.textContent = JSON.stringify(compactPayload(payload), null, 2);
    item.append(details);
  }

  els.eventLog.prepend(item);
  while (els.eventLog.children.length > MAX_LOG_ITEMS) {
    els.eventLog.lastElementChild.remove();
  }
}

function compactPayload(payload) {
  if (payload == null || typeof payload !== "object") {
    return payload;
  }
  if (Array.isArray(payload)) {
    return payload.map(compactPayload);
  }

  const result = {};
  for (const [key, value] of Object.entries(payload)) {
    if (key === "sdp" && typeof value === "string") {
      result.sdp = `${value.split(/\r?\n/).length} lines / ${value.length} chars`;
      continue;
    }
    if (key === "candidate" && typeof value === "string") {
      result.candidate =
        value.length > 90 ? `${value.slice(0, 90)}...` : value;
      continue;
    }
    if (typeof value === "object") {
      result[key] = compactPayload(value);
      continue;
    }
    result[key] = value;
  }
  return result;
}
