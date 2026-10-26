package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"inno-live-server/internal/config"
)

func TestVODDisabledNoRoutesOrBackgroundLifecycle(t *testing.T) {
	app, manager := newTestApplication(t)
	defer manager.CloseAll()
	if app.vod != nil {
		t.Fatal("disabled runtime allocated")
	}
	stop := app.startVOD(context.Background())
	stop()
	if app.vod != nil {
		t.Fatal("disabled background started")
	}
	for _, path := range []string{"/vod/jobs", "/api/v1/vod/jobs"} {
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
}
func TestVODEnabledUsesExistingUserMiddleware(t *testing.T) {
	app, manager := newTestApplication(t)
	defer manager.CloseAll()
	app.cfg.VOD = config.DefaultVOD()
	app.cfg.VOD.Enabled = true
	app.configureVOD()
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/vod/jobs", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestRecoverMiddlewarePropagatesStreamAbort(t *testing.T) {
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("abort swallowed: %v", v)
		}
	}()
	h := recoverMiddleware(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}
