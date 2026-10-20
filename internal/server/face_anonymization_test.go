package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/ai"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"

	"github.com/google/uuid"
)

// newFaceSwapTestServer는 관리자 한 명과 일반 사용자 한 명이 있는 서버를 띄운다.
func newFaceSwapTestServer(t *testing.T, enabled bool) (baseURL string, adminHeader, userHeader http.Header) {
	t.Helper()
	requireUser, authenticateUser, adminHeader, _, adminID := testRequireUser(t)
	_, _, userHeader, _, userID := testRequireUser(t)
	cfg := testServerConfig()
	cfg.AIFaceSwapEnabled = enabled
	cfg.AdminUserIDs = []string{adminID.String()}
	application, manager := newTestApplicationWithConfig(t, cfg, requireUser, authenticateUser)
	t.Cleanup(manager.CloseAll)
	application.SetPlanStore(&memoryPlanStore{plans: map[uuid.UUID]plan.Plan{adminID: plan.Spark, userID: plan.Spark}})
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer.URL, adminHeader, userHeader
}

// face_anonymization은 세션 생성에서만 고른다. 얼굴 합성은 개발 중이라 관리자만
// 고를 수 있고, 합성이 꺼진 배포는 503, 잘못된 값과 온디바이스 합성은 400이다.
func TestSessionFaceAnonymizationContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		admin   bool
		enabled bool
		mode    string
		status  int
	}{
		{"default is blur", `{}`, false, false, "blur", 201},
		{"user may choose blur", `{"face_anonymization":"blur"}`, false, true, "blur", 201},
		{"user cannot choose face swap", `{"face_anonymization":"face_swap"}`, false, true, "", 403},
		{"admin face swap", `{"face_anonymization":"face_swap"}`, true, true, "face_swap", 201},
		{"admin face swap disabled", `{"face_anonymization":"face_swap"}`, true, false, "", 503},
		{"admin on-device face swap", `{"face_anonymization":"face_swap","ai_processing":"on_device"}`, true, true, "", 400},
		{"unknown mode", `{"face_anonymization":"pixelate"}`, true, true, "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseURL, adminHeader, userHeader := newFaceSwapTestServer(t, tc.enabled)
			header := userHeader
			if tc.admin {
				header = adminHeader
			}
			res := mustRequest(t, http.MethodPost, baseURL+"/sessions", strings.NewReader(tc.body), header)
			defer res.Body.Close()
			if res.StatusCode != tc.status {
				data, _ := io.ReadAll(res.Body)
				t.Fatalf("status=%d body=%s", res.StatusCode, data)
			}
			if tc.status != 201 {
				return
			}
			var created session.Response
			if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			if created.FaceAnonymization != tc.mode {
				t.Fatalf("face_anonymization=%q, want %q", created.FaceAnonymization, tc.mode)
			}
		})
	}
}

// 합성 워커가 모두 응답 불가면 관리자도 합성 세션을 만들 수 없다(#507). 코드는 합성이
// 꺼진 배포와 같은 face_swap_unavailable이라 앱은 한 가지로 처리한다.
func TestSessionFaceSwapRejectedWhenNoFaceSwapWorkerIsReady(t *testing.T) {
	requireUser, authenticateUser, adminHeader, _, adminID := testRequireUser(t)
	cfg := testServerConfig()
	cfg.AIFaceSwapEnabled = true
	cfg.AdminUserIDs = []string{adminID.String()}
	pool, err := ai.NewPool([]string{"a:1", "swap:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := pool.SetFaceSwapTargets([]string{"swap:1"}); err != nil {
		t.Fatal(err)
	}
	pool.SetTargetReady("swap:1", false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := metrics.New()
	manager, err := session.NewManager(cfg, logger, registry, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	origins, err := origin.NewConfig(false, []string{"https://client.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	application := New(cfg, logger, registry, manager, nil, origins, requireUser, nil, authenticateUser)
	application.SetPlanStore(&memoryPlanStore{plans: map[uuid.UUID]plan.Plan{adminID: plan.Spark}})
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)

	res := mustRequest(t, http.MethodPost, httpServer.URL+"/sessions", strings.NewReader(`{"face_anonymization":"face_swap"}`), adminHeader)
	defer res.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable || body.Error.Code != "face_swap_unavailable" {
		t.Fatalf("status=%d code=%q, want 503 face_swap_unavailable", res.StatusCode, body.Error.Code)
	}
}
