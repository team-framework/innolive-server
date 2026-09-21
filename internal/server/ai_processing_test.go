package server

import (
	"inno-live-server/internal/session"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionAIProcessingContract(t *testing.T) {
	for _, tc := range []struct {
		body, mode string
		status     int
	}{
		{`{}`, "server", 201},
		{`{"ai_processing":"server"}`, "server", 201},
		{`{"ai_processing":"on_device"}`, "on_device", 201},
		{`{"ai_processing":"invalid"}`, "", 400},
		{`{"metadata":{"ai_processing":"on_device"}}`, "on_device", 201},
		{`{"ai_processing":"server","metadata":{"ai_processing":"on_device"}}`, "server", 201},
	} {
		t.Run(tc.body, func(t *testing.T) {
			app, manager := newTestApplication(t)
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
			var created struct {
				session.Response
				OwnerToken string `json:"owner_token"`
			}
			mustDecode(t, res.Body, &created)
			if created.AIProcessing != tc.mode {
				t.Fatalf("mode=%q", created.AIProcessing)
			}
			if tc.mode == "on_device" {
				if created.Media.AnonymizationEnabled {
					t.Fatal("server AI enabled for on-device input")
				}
				req, _ := http.NewRequest(http.MethodPatch, server.URL+"/sessions/"+created.SessionID+"/anonymization", strings.NewReader(`{"enabled":true}`))
				req.Header.Set("X-Session-Owner-Token", created.OwnerToken)
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode != 409 {
					t.Fatalf("toggle status=%d", response.StatusCode)
				}
			}
		})
	}
}
