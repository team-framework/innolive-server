const DEFAULT_SERVER_URL = "https://innolive.duckdns.org";
const SERVER_STORAGE_KEY = "inno-live-client.server-url";
const CLIENT_ID_STORAGE_KEY = "inno-live-client.client-id";
const MAX_LOG_ITEMS = 120;
const DEFAULT_ICE_SERVERS = [{ urls: "stun:stun.l.google.com:19302" }];
const VIDEO_TRACK_WAIT_ATTEMPTS = 15;
const VIDEO_TRACK_WAIT_INTERVAL_MS = 1000;

const els = {};
const state = {
  busy: false,
  referenceFaceBusy: false,
  referenceFaceSupported: null,
  referenceFace: null,
  referenceFacePreviewUrl: null,
  session: null,
  ownerToken: null,
  accessToken: null,
  refreshToken: null,
  authEmail: null,
  refreshPromise: null,
  signupToken: null,
  pc: null,
  ws: null,
  localStream: null,
  remoteStream: new MediaStream(),
  pollTimer: null,
  answerWaiter: null,
  candidateQueue: [],
  remoteCandidateQueue: [],
  offerSent: false,
  lastSelectedCandidatePairId: null,
  lastSessionJson: null,
};

document.addEventListener("DOMContentLoaded", () => {
  bindElements();
  initializeDefaults();
  initializeRuntimeNotice();
  bindEvents();
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
    "madeForKids",
    "autoPoll",
    "startBtn",
    "goLiveBtn",
    "pauseBroadcastBtn",
    "resumeBroadcastBtn",
    "healthBtn",
    "createSessionBtn",
    "refreshSessionsBtn",
    "errorProbeBtn",
    "disconnectBtn",
    "deleteSessionBtn",
    "broadcastSettingsState",
    "broadcastSettingsDetail",
    "broadcastTitle",
    "broadcastPrivacy",
    "broadcastCategoryId",
    "broadcastThumbnail",
    "broadcastDescription",
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
    "broadcastYoutubeAccount",
    "broadcastVideoInput",
    "broadcastRtmpState",
    "broadcastYoutubeState",
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
  els.connectYoutubeBtn.addEventListener("click", () => void connectYoutube());
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
  els.resumeBroadcastBtn.addEventListener("click", () => void resumeBroadcast());
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
      renderAuth();
      logEvent("ok", "Signed in", { email, expires_in: pair?.expires_in });
      await refreshReferenceFace({ quiet: true }).catch(() => null);
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

function clearAuthState() {
  state.accessToken = null;
  state.refreshToken = null;
  state.authEmail = null;
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
  setPill(els.authState, signedIn ? "Auth ok" : "Auth idle", signedIn ? "ok" : "idle");
  els.signInBtn.hidden = signedIn || verifying;
  els.signUpBtn.hidden = signedIn || verifying;
  els.verifyRow.hidden = !verifying;
  els.verifyBtn.hidden = !verifying;
  els.signOutBtn.hidden = !signedIn;
  // YouTube 연결은 로그인(이메일)과 별개의 부가 기능이다 — 로그인 상태에서만 노출.
  els.connectYoutubeBtn.hidden = !signedIn;
  if (!signedIn) {
    els.youtubeDetail.hidden = true;
    resetBroadcastStatus();
  }
  els.authDetail.textContent = signedIn
    ? `${state.authEmail} 로 로그인됨. 세션 API를 사용할 수 있습니다.`
    : verifying
      ? '메일로 받은 인증 코드를 입력하고 "인증 완료"를 누르세요.'
      : "로그인이 필요합니다. 계정이 없으면 이메일·비밀번호 입력 후 회원가입하세요.";
  updateButtons();
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
  setBroadcastStatus(els.broadcastYoutubeAccount, "연결 전");
  setBroadcastStatus(els.broadcastVideoInput, "WebRTC 시작 전");
  setBroadcastStatus(els.broadcastRtmpState, "시작 전");
  setBroadcastStatus(els.broadcastYoutubeState, "확인 전");
  setBroadcastStatus(els.broadcastSettingsState, "저장 전");
  els.broadcastSettingsDetail.textContent = "세션을 만든 뒤 저장할 수 있습니다.";
}

function renderBroadcastStreamStatus(stream) {
  const status = stream?.status || "";
  const attempts = Number(stream?.reconnect_attempts || 0);
  const reconnectDetail = attempts > 0 ? ` · ${attempts}회 재시도` : "";
  if (!status) {
    setBroadcastStatus(els.broadcastRtmpState, "시작 전");
    setBroadcastStatus(els.broadcastYoutubeState, "확인 전");
    return;
  }

  if (status === "streaming") {
    setBroadcastStatus(els.broadcastRtmpState, "송출 중", "ok");
    // broadcast_phase는 egress가 알 수 없는 YouTube 쪽 위치다(#142) —
    // 송출 중이어도 라이브 전환 전이면 시청자에게 보이지 않는다.
    if (stream?.broadcast_phase === "live") {
      setBroadcastStatus(els.broadcastYoutubeState, "라이브 중", "ok");
    } else if (stream?.broadcast_phase === "going_live") {
      setBroadcastStatus(els.broadcastYoutubeState, "라이브 전환 중", "warn");
    } else {
      setBroadcastStatus(els.broadcastYoutubeState, "준비됨 · 라이브 전환 대기", "warn");
    }
    return;
  }
  if (status === "reconfiguring") {
    setBroadcastStatus(els.broadcastRtmpState, "입력 규격 변경 중", "warn");
    setBroadcastStatus(els.broadcastYoutubeState, "RTMP 재구성 중", "warn");
    return;
  }
  if (status === "idle" || status === "reconnecting") {
    setBroadcastStatus(
      els.broadcastRtmpState,
      status === "reconnecting" ? `재연결 중${reconnectDetail}` : "연결 준비 중",
      "warn",
    );
    setBroadcastStatus(
      els.broadcastYoutubeState,
      status === "reconnecting" ? "RTMP 재연결 대기" : "RTMP 연결 대기",
      "warn",
    );
    return;
  }
  if (status === "paused") {
    setBroadcastStatus(els.broadcastRtmpState, "일시 중지됨", "warn");
    setBroadcastStatus(els.broadcastYoutubeState, "RTMP 연결 유지 중", "warn");
    return;
  }
  if (status === "paused_reconfiguring") {
    setBroadcastStatus(els.broadcastRtmpState, "일시 중지 준비 중", "warn");
    setBroadcastStatus(els.broadcastYoutubeState, "새 규격으로 RTMP 재구성 중", "warn");
    return;
  }
  if (status === "paused_reconnecting") {
    setBroadcastStatus(els.broadcastRtmpState, `일시 중지 화면 재연결 중${reconnectDetail}`, "warn");
    setBroadcastStatus(els.broadcastYoutubeState, "RTMP 재연결 중", "warn");
    return;
  }
  if (status === "stopped") {
    if (stream?.stop_reason === "rtmp_reconnect_exhausted") {
      setBroadcastStatus(els.broadcastRtmpState, "RTMP 재연결 실패로 종료됨", "error");
      setBroadcastStatus(els.broadcastYoutubeState, "다시 송출할 수 있습니다", "error");
      return;
    }
    if (stream?.stop_reason === "reconnect_input_timeout") {
      setBroadcastStatus(els.broadcastRtmpState, "입력 프레임 대기 시간 초과로 종료됨", "error");
      setBroadcastStatus(els.broadcastYoutubeState, "다시 송출할 수 있습니다", "error");
      return;
    }
    setBroadcastStatus(els.broadcastRtmpState, "중지됨");
    setBroadcastStatus(els.broadcastYoutubeState, "종료 반영 대기");
    return;
  }
  setBroadcastStatus(els.broadcastRtmpState, status, "warn");
  setBroadcastStatus(els.broadcastYoutubeState, "상태 확인 중", "warn");
}

// connectYoutube는 GIS 팝업으로 인가 코드를 받아 서버에 전달해 YouTube 계정을
// 연결한다. 로그인 자체는 이메일 그대로이고, 이 팝업은 송출 대상 연결 전용이다.
// 코드 교환·토큰 보관은 전부 서버 몫이라 브라우저에는 인가 코드만 스친다.
async function connectYoutube() {
  if (!state.accessToken) {
    setBroadcastStatus(els.broadcastYoutubeAccount, "로그인 필요", "error");
    setYoutubeDetail("먼저 로그인하세요.", true);
    return;
  }
  if (!window.google?.accounts?.oauth2) {
    setBroadcastStatus(els.broadcastYoutubeAccount, "연결 실패", "error");
    setYoutubeDetail("Google 스크립트를 아직 불러오지 못했습니다. 잠시 후 다시 시도하세요.", true);
    return;
  }
  let config;
  try {
    config = await apiFetch("/auth/youtube/config");
  } catch (error) {
    setBroadcastStatus(els.broadcastYoutubeAccount, "연결 설정 실패", "error");
    setYoutubeDetail(`서버에서 YouTube 연동 설정을 받지 못했습니다: ${error.message}`, true);
    return;
  }
  setBroadcastStatus(els.broadcastYoutubeAccount, "연결 중", "warn");
  setYoutubeDetail("Google 팝업에서 계정을 선택하고 동의해 주세요...");
  const codeClient = window.google.accounts.oauth2.initCodeClient({
    client_id: config.web_client_id,
    scope: config.scope,
    ux_mode: "popup",
    callback: (response) => {
      if (!response.code) {
        setBroadcastStatus(els.broadcastYoutubeAccount, "연결 실패", "error");
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
          setBroadcastStatus(els.broadcastYoutubeAccount, title, "ok");
          logEvent("ok", "YouTube account connected", result);
        } catch (error) {
          setBroadcastStatus(els.broadcastYoutubeAccount, "연결 실패", "error");
          setYoutubeDetail(`연결 실패: ${error.message}`, true);
          logEvent("error", "YouTube connect failed", { message: error.message });
        }
      })();
    },
    error_callback: (error) => {
      setBroadcastStatus(els.broadcastYoutubeAccount, "연결 실패", "error");
      setYoutubeDetail(`Google 팝업 오류: ${error?.type || JSON.stringify(error)}`, true);
    },
  });
  codeClient.requestCode();
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
  const session = await apiFetch("/sessions", {
    method: "POST",
    body: JSON.stringify({ metadata }),
  });
  // owner_token은 여기서 정확히 한 번만 반환된다. 이후 세션 범위 요청과 signaling이
  // 소유권을 증명하도록 메모리에 보관하며, 세션 새로고침 응답에는 다시 오지 않는다.
  state.ownerToken = session.owner_token || null;
  logEvent("ok", "Session created", {
    session_id: session.session_id,
    metadata: session.metadata,
  });
  await applyBroadcastDefaults(session.session_id);
  return session;
}

// applyBroadcastDefaults는 직전 방송 값을 폼에 채운다(#143). 이미 입력해 둔
// 값은 덮지 않고, 조회가 실패해도 서버가 폴백값을 주므로 폼은 그대로 쓴다.
async function applyBroadcastDefaults(sessionId) {
  try {
    const defaults = await apiFetch(`/sessions/${sessionId}/broadcast/defaults`);
    if (!els.broadcastTitle.value.trim()) {
      els.broadcastTitle.value = defaults.title || "";
    }
    if (!els.broadcastDescription.value) {
      els.broadcastDescription.value = defaults.description || "";
    }
    if (!els.broadcastCategoryId.value.trim()) {
      els.broadcastCategoryId.value = defaults.category_id || "";
    }
    if (defaults.privacy) {
      els.broadcastPrivacy.value = defaults.privacy;
    }
    // 아동용 여부는 미선택(null)이면 사용자가 직접 골라야 하므로 두고 본다.
    if (typeof defaults.made_for_kids === "boolean") {
      els.madeForKids.checked = defaults.made_for_kids;
    }
    els.broadcastSettingsDetail.textContent =
      "직전 방송 값을 불러왔습니다. 확인 후 저장하세요.";
    logEvent("ok", "Broadcast defaults loaded", { session_id: sessionId, defaults });
  } catch (error) {
    logEvent("warn", "Broadcast defaults load failed", {
      session_id: sessionId,
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
  if (!state.session?.session_id) {
    return null;
  }

  try {
    const session = await apiFetch(`/sessions/${state.session.session_id}`);
    setCurrentSession(session);
    if (!quiet) {
      logEvent("ok", "Session refreshed", { session_id: session.session_id });
    }
    return session;
  } catch (error) {
    if (error.status === 404) {
      logEvent("warn", "Current session no longer exists", {
        session_id: state.session.session_id,
      });
      clearCurrentSession();
      stopPolling();
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
        logEvent("ok", "Reusing active WebRTC connection for YouTube broadcast", {
          session_id: state.session.session_id,
        });
        const readySession = await waitForVideoTrack();
        await prepareYouTubeBroadcast(readySession);
        return;
      }

      const reusableSession = canUseCurrentSessionForOffer();
      if (!reusableSession) {
        await cleanupConnection({ keepSession: false });
      }

      await ensureLocalMedia();
      if (!reusableSession) {
        setCurrentSession(await createSession());
      }
      setBroadcastStatus(els.broadcastVideoInput, "WebRTC 연결 중", "warn");
      setBroadcastStatus(els.broadcastRtmpState, "영상 입력 대기", "warn");
      setBroadcastStatus(els.broadcastYoutubeState, "방송 준비 대기", "warn");
      await connectPeer(state.session.session_id);
      startPolling();
      await refreshCurrentSession({ quiet: true });
      await refreshSessions({ quiet: true });

      const readySession = await waitForVideoTrack();
      await prepareYouTubeBroadcast(readySession);
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
  const [file] = els.broadcastThumbnail.files;
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

// saveBroadcastSettings는 폼 값을 PUT /sessions/{id}/broadcast로 저장한다.
// PUT은 전체 교체라 비운 필드는 서버에서도 비워진다.
async function saveBroadcastSettings() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("방송 설정을 저장할 세션이 없습니다.");
  }
  const payload = {
    title: els.broadcastTitle.value.trim(),
    description: els.broadcastDescription.value,
    privacy: els.broadcastPrivacy.value,
    made_for_kids: els.madeForKids.checked,
    category_id: els.broadcastCategoryId.value.trim(),
    thumbnail: await readBroadcastThumbnail(),
  };
  try {
    const updated = await apiFetch(`/sessions/${sessionId}/broadcast`, {
      method: "PUT",
      body: JSON.stringify(payload),
    });
    setCurrentSession(updated);
    setBroadcastStatus(els.broadcastSettingsState, "저장됨", "ok");
    els.broadcastSettingsDetail.textContent = describeBroadcastSettings(updated.broadcast);
    logEvent("ok", "Broadcast settings saved", {
      session_id: sessionId,
      broadcast: updated.broadcast,
    });
    return updated;
  } catch (error) {
    const details = error?.payload?.error?.details;
    setBroadcastStatus(els.broadcastSettingsState, "저장 실패", "error");
    els.broadcastSettingsDetail.textContent = details?.field
      ? `${details.field}: ${details.reason || error.message}`
      : error.message;
    logEvent("error", "Broadcast settings save failed", {
      session_id: sessionId,
      field: details?.field,
      message: error?.message,
      status: error?.status,
    });
    throw error;
  }
}

function describeBroadcastSettings(broadcast) {
  if (!broadcast) {
    return "저장된 설정이 없습니다.";
  }
  const parts = [
    broadcast.title || "제목 없음",
    broadcast.privacy || "privacy 미설정",
    broadcast.category_id ? `카테고리 ${broadcast.category_id}` : "카테고리 없음",
    broadcast.thumbnail ? `썸네일 ${formatBytes(broadcast.thumbnail.bytes)}` : "썸네일 없음",
  ];
  return parts.join(" · ");
}

async function prepareYouTubeBroadcast(session) {
  const sessionId = session?.session_id;
  if (!sessionId) {
    throw new Error("YouTube 방송을 준비할 세션이 없습니다.");
  }

  setBroadcastStatus(els.broadcastRtmpState, "방송 준비 중", "warn");
  setBroadcastStatus(els.broadcastYoutubeState, "YouTube 준비 중", "warn");
  logEvent("ok", "YouTube broadcast prepare requested", { session_id: sessionId });
  try {
    // 준비 옵션은 저장된 설정이 단일 출처이므로 준비 직전에 폼을 반영한다.
    await saveBroadcastSettings();
    const prepared = await apiFetch(`/sessions/${sessionId}/stream/prepare`, {
      method: "POST",
      body: JSON.stringify({ provider: "youtube" }),
    });
    setCurrentSession(prepared);
    renderBroadcastStreamStatus(prepared.stream);
    renderBroadcastWarnings(prepared.warnings);
    logEvent("ok", "YouTube broadcast prepared", {
      session_id: sessionId,
      stream: prepared.stream,
    });
  } catch (error) {
    renderBroadcastStartError(error);
    logEvent("error", "YouTube broadcast prepare failed", {
      session_id: sessionId,
      code: error?.payload?.error?.code,
      message: error?.message,
      status: error?.status,
    });
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
    setBroadcastStatus(els.broadcastYoutubeState, "라이브 전환 중", "warn");
    try {
      const stream = await apiFetch(`/sessions/${sessionId}/stream/golive`, {
        method: "POST",
      });
      renderBroadcastStreamStatus(stream);
      setBroadcastStatus(els.broadcastYoutubeState, "라이브", "ok");
      logEvent("ok", "YouTube broadcast is live", { session_id: sessionId, stream });
      await refreshCurrentSession({ quiet: true });
    } catch (error) {
      renderBroadcastStartError(error);
      logEvent("error", "YouTube go live failed", {
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
    const paused = await apiFetch(`/sessions/${sessionId}/stream/pause`, {
      method: "POST",
    });
    updateCurrentSessionStream(paused);
    logEvent("ok", "YouTube broadcast pause requested", {
      session_id: sessionId,
      stream: paused,
    });
  });
}

async function resumeBroadcast() {
  const sessionId = state.session?.session_id;
  if (!sessionId) {
    throw new Error("재개할 방송 세션이 없습니다.");
  }
  await runBusy(async () => {
    const resumed = await apiFetch(`/sessions/${sessionId}/stream/resume`, {
      method: "POST",
    });
    updateCurrentSessionStream(resumed);
    logEvent("ok", "YouTube broadcast resume requested", {
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
    .map((warning) => warning.message)
    .join(" / ");
  logEvent("warn", "Broadcast settings partially applied", { warnings });
}

function renderBroadcastStartError(error) {
  const code = error?.payload?.error?.code;
  if (code === "streaming_not_connected") {
    setBroadcastStatus(els.broadcastYoutubeAccount, "연결 필요", "warn");
    setBroadcastStatus(els.broadcastRtmpState, "시작 안 함");
    setBroadcastStatus(els.broadcastYoutubeState, "YouTube 계정 연결 필요", "warn");
    return;
  }
  if (code === "streaming_reconnect_required") {
    setBroadcastStatus(els.broadcastYoutubeAccount, "재연결 필요", "warn");
    setBroadcastStatus(els.broadcastRtmpState, "시작 안 함");
    setBroadcastStatus(els.broadcastYoutubeState, "YouTube 재연결 필요", "warn");
    return;
  }
  if (code === "live_streaming_blocked") {
    setBroadcastStatus(els.broadcastRtmpState, "시작 안 함");
    setBroadcastStatus(els.broadcastYoutubeState, "채널 라이브 권한 없음", "error");
    return;
  }
  if (code === "broadcast_not_ready") {
    // 송출 프레임이 YouTube에 아직 도착하지 않은 상태 — 잠시 후 재시도.
    setBroadcastStatus(els.broadcastYoutubeState, "라이브 전환 대기 · 잠시 후 재시도", "warn");
    return;
  }
  if (code === "broadcast_prepared" || code === "broadcast_live") {
    setBroadcastStatus(els.broadcastYoutubeState, "이미 준비된 방송이 있음", "warn");
    return;
  }
  if (code === "broadcast_going_live") {
    setBroadcastStatus(els.broadcastYoutubeState, "라이브 전환 중", "warn");
    return;
  }
  if (code === "broadcast_stopped") {
    // 전환 왕복 중에 중지가 들어와 중지가 이긴 경우다(#142).
    setBroadcastStatus(els.broadcastYoutubeState, "중지되어 라이브 취소됨", "warn");
    return;
  }
  if (code === "stream_already_active") {
    setBroadcastStatus(els.broadcastRtmpState, "이미 송출 중", "warn");
    setBroadcastStatus(els.broadcastYoutubeState, "기존 방송 사용 중 · live 확인 전", "warn");
    return;
  }
  setBroadcastStatus(els.broadcastRtmpState, "시작 실패", "error");
  setBroadcastStatus(els.broadcastYoutubeState, "YouTube 준비 실패", "error");
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

function buildVideoConstraints() {
  const selectedCamera = els.cameraSelect.value;
  const resolution = els.resolutionSelect.value;
  const video = {};

  if (selectedCamera) {
    video.deviceId = { exact: selectedCamera };
  }
  // FHD는 AI FHD 지원(innolive-ai#4) 전까지 비활성화한다. index.html option과
  // 함께 지원 배포 뒤 다시 활성화한다.
  // if (resolution === "fhd") {
  //   video.width = { ideal: 1920 };
  //   video.height = { ideal: 1080 };
  // }
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
  state.lastSelectedCandidatePairId = null;

  const iceServers = await loadIceServers();
  const pc = new RTCPeerConnection({ iceServers });
  state.pc = pc;
  wirePeerConnection(pc);

  addLocalTracks(pc);

  const ws = await openSignalingSocket();
  state.ws = ws;
  const answerPromise = waitForAnswer(sessionId);

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  updatePeerUi();

  sendSignaling({
    type: "offer",
    session_id: sessionId,
    owner_token: state.ownerToken,
    access_token: state.accessToken,
    sdp: pc.localDescription.sdp,
  });
  state.offerSent = true;
  flushCandidateQueue();

  const answer = await answerPromise;
  await pc.setRemoteDescription({
    type: "answer",
    sdp: answer.sdp,
  });
  await flushRemoteCandidateQueue();
  updatePeerUi();
  logEvent("ok", "Remote answer applied", {
    session_id: answer.session_id,
    sdp_lines: answer.sdp.split(/\r?\n/).length,
  });
}

async function loadIceServers() {
  try {
    const payload = await apiFetch("/webrtc/config");
    const iceServers = Array.isArray(payload?.iceServers)
      ? payload.iceServers
      : [];
    if (!iceServers.length) {
      logEvent("warn", "Server returned no ICE servers; using host candidates only");
      return [];
    }
    logEvent("ok", "Loaded ICE server configuration", {
      urls: iceServers.map((server) => server.urls),
    });
    return iceServers;
  } catch (error) {
    logError("Failed to load ICE server configuration; using default STUN", error);
    return DEFAULT_ICE_SERVERS;
  }
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

function waitForAnswer(sessionId) {
  if (state.answerWaiter) {
    rejectWaitingAnswer(new Error("Replaced pending answer waiter."));
  }

  return new Promise((resolve, reject) => {
    const timeout = window.setTimeout(() => {
      rejectWaitingAnswer(new Error("Timed out waiting for WebRTC answer."));
    }, 15000);

    state.answerWaiter = {
      sessionId,
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
    logEvent("ok", "Received WebRTC answer", payload);
    if (
      state.answerWaiter &&
      state.answerWaiter.sessionId === payload.session_id
    ) {
      state.answerWaiter.resolve(payload);
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
      rejectWaitingAnswer(
        new Error(payload.error?.message || "Signaling error response."),
      );
    }
    return;
  }

  logEvent("warn", "Received unknown signaling message", payload);
}

async function addRemoteCandidate(payload) {
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

  if (!state.pc.remoteDescription) {
    state.remoteCandidateQueue.push(candidateInit);
    logEvent("warn", "Queued remote ICE candidate until remote description is set", {
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

async function flushRemoteCandidateQueue() {
  while (state.remoteCandidateQueue.length) {
    const candidateInit = state.remoteCandidateQueue.shift();
    await state.pc.addIceCandidate(candidateInit);
    logEvent("ok", "Added queued remote ICE candidate", {
      end_of_candidates: candidateInit === null,
      ...describeCandidateString(candidateInit?.candidate),
    });
  }
}

function queueOrSendCandidate(candidate) {
  const payload = candidate
    ? {
        type: "ice_candidate",
        session_id: state.session?.session_id,
        owner_token: state.ownerToken,
        access_token: state.accessToken,
        candidate: candidate.candidate,
        sdpMid: candidate.sdpMid,
        sdpMLineIndex: candidate.sdpMLineIndex,
      }
    : {
        type: "ice_candidate",
        session_id: state.session?.session_id,
        owner_token: state.ownerToken,
        access_token: state.accessToken,
        candidate: null,
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

async function cleanupConnection({ keepSession }) {
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
  state.lastSelectedCandidatePairId = null;
  setPill(els.websocketState, "WS idle", "idle");
  updatePeerUi();

  if (!keepSession) {
    clearCurrentSession();
  }
  updateButtons();
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
  updateButtons();
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
  els.startBtn.disabled = state.busy || fileProtocol || !signedIn;
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
