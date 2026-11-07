package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"inno-live-server/internal/auth"
)

func TestVODTransferDoesNotBlockWithdrawal(t *testing.T) {
	requireUser, _, accessHeader, _, userID := testRequireUser(t)
	gate := auth.NewUserOperationGate()
	application := &Server{userOperationGate: gate}

	started := make(chan struct{})
	finished := make(chan struct{})
	transfer := requireUser(application.withCancellableUserOperation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// 긴 업로드·다운로드를 흉내 낸다. 요청 컨텍스트가 끝날 때까지 놓지 않는다.
		<-r.Context().Done()
	})))
	go func() {
		defer close(finished)
		request := httptest.NewRequest(http.MethodGet, "/vod/jobs/x/result", nil)
		request.Header = accessHeader.Clone()
		transfer.ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := gate.BeginWithdrawal(ctx, userID)
	if err != nil {
		t.Fatalf("withdrawal waited on an in-flight VOD transfer: %v", err)
	}
	release()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("VOD transfer kept running after withdrawal started")
	}
}

func TestVODRouteRejectedDuringWithdrawal(t *testing.T) {
	requireUser, _, accessHeader, _, userID := testRequireUser(t)
	gate := auth.NewUserOperationGate()
	application := &Server{userOperationGate: gate}
	release, err := gate.BeginWithdrawal(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	called := false
	handler := requireUser(application.withCancellableUserOperation(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})))
	request := httptest.NewRequest(http.MethodPost, "/vod/jobs", nil)
	request.Header = accessHeader.Clone()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if called || recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, called = %v; want 409 without calling the handler", recorder.Code, called)
	}
}
