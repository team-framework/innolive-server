package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateSessionBroadcastResolution(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		res  string
	}{
		{"fhd", `{"broadcast_resolution":"fhd"}`, http.StatusCreated, "fhd"},
		{"omitted defaults to 720p", `{}`, http.StatusCreated, "720p"},
		{"unsupported value", `{"broadcast_resolution":"1080p"}`, http.StatusBadRequest, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			requireUser, authenticateUser, header, _, _ := testRequireUser(t)
			application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
			defer manager.CloseAll()
			httpServer := httptest.NewServer(application.Handler())
			defer httpServer.Close()

			response := mustRequest(t, http.MethodPost, httpServer.URL+"/sessions", strings.NewReader(test.body), header)
			defer response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
			}
			if test.want != http.StatusCreated {
				return
			}
			var created struct {
				BroadcastResolution string `json:"broadcast_resolution"`
			}
			if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			if created.BroadcastResolution != test.res {
				t.Fatalf("broadcast_resolution = %q, want %q", created.BroadcastResolution, test.res)
			}
		})
	}
}
