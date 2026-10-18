package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type mosaicConfigBody struct {
	BlurRadius *float64 `json:"blur_radius"`
	PixelSize  *uint32  `json:"pixel_size"`
	MaskMargin *uint32  `json:"mask_margin"`
}

func createMosaicTestSession(t *testing.T, baseURL, mode string) (string, string, mosaicConfigBody) {
	t.Helper()
	response, err := http.Post(baseURL+"/sessions", "application/json", strings.NewReader(`{"ai_processing":"`+mode+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var created struct {
		SessionID  string `json:"session_id"`
		OwnerToken string `json:"owner_token"`
		Media      struct {
			MosaicConfig mosaicConfigBody `json:"mosaic_config"`
		} `json:"media"`
	}
	mustDecode(t, response.Body, &created)
	return created.SessionID, created.OwnerToken, created.Media.MosaicConfig
}

func putMosaicConfig(t *testing.T, url, owner, body string, cookies ...*http.Cookie) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Session-Owner-Token", owner)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return response.StatusCode, decoded
}

// mosaicConfigOf는 세션 응답의 media.mosaic_config를 꺼낸다. 값이 null이면 키는
// 있고 값이 nil이다.
func mosaicConfigOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	media, _ := body["media"].(map[string]any)
	config, ok := media["mosaic_config"].(map[string]any)
	if !ok {
		t.Fatalf("response has no media.mosaic_config: %v", body)
	}
	return config
}

func TestPutMosaicConfigStoresValuesAndKeepsZeroDistinctFromUnset(t *testing.T) {
	app, manager := newTestApplication(t)
	defer manager.CloseAll()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	id, owner, initial := createMosaicTestSession(t, server.URL, "server")
	if initial.BlurRadius != nil || initial.PixelSize != nil || initial.MaskMargin != nil {
		t.Fatalf("new session mosaic_config = %+v, want all null", initial)
	}
	url := server.URL + "/sessions/" + id + "/mosaic-config"

	status, body := putMosaicConfig(t, url, owner, `{"blur_radius":32.5,"pixel_size":1,"mask_margin":0}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	got := mosaicConfigOf(t, body)
	if got["blur_radius"] != 32.5 || got["pixel_size"] != float64(1) || got["mask_margin"] != float64(0) {
		t.Fatalf("mosaic_config = %v", got)
	}

	// 설정 전체를 바꾸므로 빠진 필드와 null은 AI 기본값(null)으로 돌아간다.
	status, body = putMosaicConfig(t, url, owner, `{"mask_margin":16,"pixel_size":null}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	got = mosaicConfigOf(t, body)
	if got["blur_radius"] != nil || got["pixel_size"] != nil || got["mask_margin"] != float64(16) {
		t.Fatalf("mosaic_config after replace = %v", got)
	}
}

func TestPutMosaicConfigRejectsOutOfRangeWithoutChangingSession(t *testing.T) {
	app, manager := newTestApplication(t)
	defer manager.CloseAll()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	id, owner, _ := createMosaicTestSession(t, server.URL, "server")
	url := server.URL + "/sessions/" + id + "/mosaic-config"
	if status, body := putMosaicConfig(t, url, owner, `{"mask_margin":8}`); status != http.StatusOK {
		t.Fatalf("seed status=%d body=%v", status, body)
	}

	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"negative margin", `{"mask_margin":-1}`, "mask_margin"},
		{"fractional margin", `{"mask_margin":1.5}`, "mask_margin"},
		{"string margin", `{"mask_margin":"8"}`, "mask_margin"},
		{"margin above 64", `{"mask_margin":65}`, "mask_margin"},
		{"zero radius", `{"blur_radius":0}`, "blur_radius"},
		{"radius above 64", `{"blur_radius":64.5}`, "blur_radius"},
		{"radius overflow", `{"blur_radius":1e400}`, "blur_radius"},
		{"zero pixel size", `{"pixel_size":0}`, "pixel_size"},
		{"pixel size above 8", `{"pixel_size":9}`, "pixel_size"},
		{"unknown field", `{"mask_margin":8,"feather":2}`, ""},
		{"two objects", `{"mask_margin":8}{"mask_margin":9}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := putMosaicConfig(t, url, owner, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%v", status, body)
			}
			if tc.field != "" {
				details, _ := body["error"].(map[string]any)["details"].(map[string]any)
				if details["field"] != tc.field {
					t.Fatalf("error details = %v, want field %s", details, tc.field)
				}
			}
			live, err := manager.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			stored := live.Response().Media.MosaicConfig
			if stored.MaskMargin == nil || *stored.MaskMargin != 8 || stored.BlurRadius != nil || stored.PixelSize != nil {
				t.Fatalf("rejected request changed the session config: %+v", stored)
			}
		})
	}
}

func TestPutMosaicConfigRejectsOnDeviceSession(t *testing.T) {
	app, manager := newTestApplication(t)
	defer manager.CloseAll()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	id, owner, _ := createMosaicTestSession(t, server.URL, "on_device")
	status, body := putMosaicConfig(t, server.URL+"/sessions/"+id+"/mosaic-config", owner, `{"mask_margin":16}`)
	if status != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%v", status, body)
	}
}

func TestGuestPutMosaicConfigRequiresOwningGuest(t *testing.T) {
	app, manager := newTestApplication(t)
	defer manager.CloseAll()
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	app.SetGuestQueue(&GuestQueue{client: client, sessions: manager, ttl: time.Minute, admissionTTL: time.Minute, maxGuests: 1})
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	guest := "guest-cookie"
	live, owner, err := manager.CreateForGuest(guestHash(guest), nil)
	if err != nil {
		t.Fatal(err)
	}
	url := server.URL + "/guest/sessions/" + live.ID + "/mosaic-config"
	if status, _ := putMosaicConfig(t, url, owner, `{"mask_margin":16}`, &http.Cookie{Name: guestCookieName, Value: "other-guest"}); status != http.StatusForbidden {
		t.Fatalf("other guest status=%d, want 403", status)
	}
	status, body := putMosaicConfig(t, url, owner, `{"mask_margin":16}`, &http.Cookie{Name: guestCookieName, Value: guest})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if got := mosaicConfigOf(t, body)["mask_margin"]; got != float64(16) {
		t.Fatalf("mask_margin = %v, want 16", got)
	}
	if status, _ := putMosaicConfig(t, url, owner, `{"mask_margin":65}`, &http.Cookie{Name: guestCookieName, Value: guest}); status != http.StatusBadRequest {
		t.Fatalf("out-of-range guest status=%d, want 400", status)
	}
	if got := live.Response().Media.MosaicConfig.MaskMargin; got == nil || *got != 16 {
		t.Fatalf("stored mask_margin = %v, want 16", got)
	}
}
