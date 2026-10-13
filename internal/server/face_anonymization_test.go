package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
