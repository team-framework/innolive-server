package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"inno-live-server/internal/session"
)

// face_anonymization은 세션 생성에서만 고른다. 합성이 꺼진 배포는 503, 잘못된
// 값과 온디바이스 합성은 400이다.
func TestSessionFaceAnonymizationContract(t *testing.T) {
	for _, tc := range []struct {
		body    string
		enabled bool
		mode    string
		status  int
	}{
		{`{}`, false, "blur", 201},
		{`{"face_anonymization":"blur"}`, false, "blur", 201},
		{`{"face_anonymization":"face_swap"}`, false, "", 503},
		{`{"face_anonymization":"face_swap"}`, true, "face_swap", 201},
		{`{"face_anonymization":"face_swap","ai_processing":"on_device"}`, true, "", 400},
		{`{"face_anonymization":"pixelate"}`, true, "", 400},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cfg := testServerConfig()
			cfg.RequireSessionAuth = false
			cfg.AIFaceSwapEnabled = tc.enabled
			app, manager := newTestApplicationWithConfig(t, cfg, nil)
			defer manager.CloseAll()
			server := httptest.NewServer(app.Handler())
			defer server.Close()
			res, err := http.Post(server.URL+"/sessions", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.status {
				data, _ := io.ReadAll(res.Body)
				t.Fatalf("status=%d body=%s", res.StatusCode, data)
			}
			if tc.status != 201 {
				return
			}
			var created session.Response
			mustDecode(t, res.Body, &created)
			if created.FaceAnonymization != tc.mode {
				t.Fatalf("face_anonymization=%q, want %q", created.FaceAnonymization, tc.mode)
			}
		})
	}
}
