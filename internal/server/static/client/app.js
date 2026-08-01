const DEFAULT_SERVER_URL = "https://innolive.duckdns.org";
const SERVER_STORAGE_KEY = "inno-live-client.server-url";
const CLIENT_ID_STORAGE_KEY = "inno-live-client.client-id";
const MAX_LOG_ITEMS = 120;
const DEFAULT_ICE_SERVERS = [{ urls: "stun:stun.l.google.com:19302" }];

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
  // Reference-face status is behind RequireUser; fetch it only once signed in.
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
    "healthBtn",
    "createSessionBtn",
    "refreshSessionsBtn",
    "errorProbeBtn",
    "disconnectBtn",
    "deleteSessionBtn",
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
  // User identity: the JWT access token proves an active InnoLive user and is
  // required by RequireUser on every session and reference-face route.
  if (state.accessToken && !headers.has("Authorization")) {
    headers.set("Authorization", `Bearer ${state.accessToken}`);
  }
  // Session ownership: the owner token minted once at session creation goes in
  // its own header. Attaching it to non-session routes is harmless.
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
    // A 401 means the access token is missing or expired. Try one silent
    // refresh with the stored refresh token and replay the request; only if
    // that fails do we drop the session so the UI re-gates on sign-in.
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
      // runBusy logs the failure; re-throw so it can surface it once.
      throw error;
    }
  });
}

// requestSignup starts the email registration flow: the server emails a code
// and returns a signup_token that pairs with it. The native endpoint returns
// that token in the body so the browser test client does not depend on cookies.
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
      throw error; // runBusy logs it once
    }
  });
}

// verifySignup confirms the emailed code, then signs in with the same
// credentials so the tester lands in an authenticated state in one step.
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
      throw error; // runBusy logs it once
    }
  });
  if (!state.signupToken) {
    await signIn();
  }
}

async function signOut() {
  // Tear down any live connection before dropping the session it belongs to,
  // so a mid-session sign-out does not leave the peer connection running.
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

// refreshAccessToken exchanges the stored refresh token for a fresh access
// token. It calls fetch directly (not apiFetch) so a 401 from /auth/refresh
// cannot recurse back into refresh, and it de-duplicates concurrent callers
// (the 2s session poll can fire several 401s at once) via a shared promise.
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

// renderAuth reflects the three auth modes and shows only each mode's controls.
// The verification code field appears solely while entering the emailed code —
// never on the login screen or when first requesting the code.
function renderAuth() {
  const signedIn = Boolean(state.accessToken);
  const verifying = Boolean(state.signupToken);
  setPill(els.authState, signedIn ? "Auth ok" : "Auth idle", signedIn ? "ok" : "idle");
  els.signInBtn.hidden = signedIn || verifying;
  els.signUpBtn.hidden = signedIn || verifying;
  els.verifyRow.hidden = !verifying;
  els.verifyBtn.hidden = !verifying;
  els.signOutBtn.hidden = !signedIn;
  els.authDetail.textContent = signedIn
    ? `${state.authEmail} 로 로그인됨. 세션 API를 사용할 수 있습니다.`
    : verifying
      ? '메일로 받은 인증 코드를 입력하고 "인증 완료"를 누르세요.'
      : "로그인이 필요합니다. 계정이 없으면 이메일·비밀번호 입력 후 회원가입하세요.";
  updateButtons();
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
  // owner_token is returned exactly once, here. Keep it in memory so later
  // session-scoped requests (and signaling) can prove ownership; a session
  // refresh response will not carry it again.
  state.ownerToken = session.owner_token || null;
  logEvent("ok", "Session created", {
    session_id: session.session_id,
    metadata: session.metadata,
  });
  return session;
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
  // The GET /sessions listing endpoint was removed server-side (it leaked every
  // active session_id). This viewer only owns the session it created, so the
  // panel now reflects just the current session.
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

      const reusableSession = canUseCurrentSessionForOffer();
      if (!reusableSession) {
        await cleanupConnection({ keepSession: false });
      }

      await ensureLocalMedia();
      if (!reusableSession) {
        setCurrentSession(await createSession());
      }
      await connectPeer(state.session.session_id);
      startPolling();
      await refreshCurrentSession({ quiet: true });
      await refreshSessions({ quiet: true });
    } catch (error) {
      await cleanupConnection({ keepSession: Boolean(state.session) });
      throw error;
    }
  });
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

  els.signInBtn.disabled = state.busy;
  els.signUpBtn.disabled = state.busy;
  els.verifyBtn.disabled = state.busy;
  els.signOutBtn.disabled = state.busy;
  // Session APIs are behind RequireUser, so gate them on a signed-in user.
  els.startBtn.disabled = state.busy || fileProtocol || !signedIn;
  els.healthBtn.disabled = state.busy;
  els.createSessionBtn.disabled = state.busy || !signedIn;
  els.refreshSessionsBtn.disabled = state.busy;
  els.disconnectBtn.disabled = state.busy || !hasConnection;
  els.deleteSessionBtn.disabled = state.busy || !hasSession;
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
