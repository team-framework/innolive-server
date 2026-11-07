package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthRequestDecodersRejectNonStrictJSON(t *testing.T) {
	cases := []struct {
		name   string
		limit  int
		valid  string
		decode func(http.ResponseWriter, *http.Request) error
	}{
		{"refresh", 8 << 10, `{"refresh_token":"t"}`, func(w http.ResponseWriter, r *http.Request) error {
			_, err := decodeRefreshRequest(w, r)
			return err
		}},
		{"google", 16 << 10, `{"id_token":"t"}`, func(w http.ResponseWriter, r *http.Request) error {
			_, err := decodeGoogleLoginRequest(w, r)
			return err
		}},
		{"apple", 16 << 10, `{"authorization_code":"c","nonce":"n"}`, func(w http.ResponseWriter, r *http.Request) error {
			_, err := decodeAppleLoginRequest(w, r)
			return err
		}},
		{"chzzk", 16 << 10, `{"code":"c","state":"s"}`, func(w http.ResponseWriter, r *http.Request) error {
			_, _, err := decodeChzzkConnectRequest(w, r)
			return err
		}},
		{"youtube", 16 << 10, `{"server_auth_code":"c"}`, func(w http.ResponseWriter, r *http.Request) error {
			_, _, err := decodeYouTubeConnectRequest(w, r)
			return err
		}},
		{"email login", 16 << 10, `{"email":"a@example.com","password":"p"}`, func(w http.ResponseWriter, r *http.Request) error {
			_, err := decodeEmailLoginRequest(w, r)
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(body string) error {
				r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				return tc.decode(httptest.NewRecorder(), r)
			}
			pad := func(size int) string {
				return tc.valid + strings.Repeat(" ", size-len(tc.valid))
			}
			unknown := strings.TrimSuffix(tc.valid, "}") + `,"extra":true}`

			if err := run(tc.valid); err != nil {
				t.Fatalf("valid body rejected: %v", err)
			}
			if err := run(pad(tc.limit)); err != nil {
				t.Fatalf("body of exactly %d bytes rejected: %v", tc.limit, err)
			}
			if run(pad(tc.limit+1)) == nil {
				t.Fatalf("body of %d bytes accepted", tc.limit+1)
			}
			if run(unknown) == nil {
				t.Fatal("unknown field accepted")
			}
			if run(tc.valid+tc.valid) == nil {
				t.Fatal("second JSON object accepted")
			}
			if run("") == nil {
				t.Fatal("empty body accepted")
			}
		})
	}
}
