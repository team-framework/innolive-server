package server

import (
	"inno-live-server/internal/session"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScreenBlurToggleDefaultsOffAndRejectsOnDevice(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		status int
	}{
		{"server", http.StatusOK},
		{"on_device", http.StatusConflict},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			app, manager := newTestApplication(t)
			defer manager.CloseAll()
			server := httptest.NewServer(app.Handler())
			defer server.Close()
			res, err := http.Post(server.URL+"/sessions", "application/json", strings.NewReader(`{"ai_processing":"`+tc.mode+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var created struct {
				session.Response
				OwnerToken string `json:"owner_token"`
			}
			mustDecode(t, res.Body, &created)
			if created.Media.ScreenBlurEnabled {
				t.Fatal("screen blur must default to off")
			}

			req, _ := http.NewRequest(http.MethodPatch, server.URL+"/sessions/"+created.SessionID+"/screen-blur", strings.NewReader(`{"enabled":true}`))
			req.Header.Set("X-Session-Owner-Token", created.OwnerToken)
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("toggle status=%d, want %d", response.StatusCode, tc.status)
			}
			if tc.status != http.StatusOK {
				return
			}
			var toggled session.Response
			mustDecode(t, response.Body, &toggled)
			if !toggled.Media.ScreenBlurEnabled {
				t.Fatal("screen blur must be enabled after toggle")
			}
		})
	}
}

func TestScreenBlurToggleRequiresEnabled(t *testing.T) {
	app, manager := newTestApplication(t)
	defer manager.CloseAll()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	res, err := http.Post(server.URL+"/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var created struct {
		session.Response
		OwnerToken string `json:"owner_token"`
	}
	mustDecode(t, res.Body, &created)
	req, _ := http.NewRequest(http.MethodPatch, server.URL+"/sessions/"+created.SessionID+"/screen-blur", strings.NewReader(`{}`))
	req.Header.Set("X-Session-Owner-Token", created.OwnerToken)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", response.StatusCode)
	}
}
