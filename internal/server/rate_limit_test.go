package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"
)

// 라이브 전환의 요청 한도도 준비와 같은 429로 알린다(#505). 일반 전환 실패(502)로
// 내리면 클라이언트가 "잠시 뒤·다른 채널" 안내를 할 수 없다.
func TestGoLiveReportsPlatformRateLimit(t *testing.T) {
	youtube := &stubStreamingProvider{goLiveErr: streaming.ErrPlatformRateLimited}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	markPrepared(t, manager, created.SessionID, auth.StreamingProviderYouTube, "")

	response, payload := goLive(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusTooManyRequests || streamErrorCode(payload) != "streaming_rate_limited" {
		t.Fatalf("go live = %d %q, want 429 streaming_rate_limited", response.StatusCode, streamErrorCode(payload))
	}
	session := getSessionPayload(t, server.URL, created.SessionID, ownerToken)
	if phase := targetBroadcastPhase(t, session, string(auth.StreamingProviderYouTube)); phase != "prepared" {
		t.Fatalf("youtube phase = %q, want prepared restored", phase)
	}
}

// 동시 송출에서 요청 한도에 걸린 대상은 failed_targets에 그 사유를 싣는다(#505).
func TestGoLiveFailedTargetCarriesRateLimit(t *testing.T) {
	youtube := &stubStreamingProvider{goLiveErr: streaming.ErrPlatformRateLimited}
	chzzk := &stubStreamingProvider{}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
		auth.StreamingProviderChzzk:   chzzk,
	})
	// 온디바이스 세션은 치지직 전환에 egress를 붙이지 않아 트랙 없이 성공 대상을 세울 수 있다.
	sessionID, ownerToken := onDeviceStreamSession(t, server.URL)
	markPrepared(t, manager, sessionID, auth.StreamingProviderYouTube, "")
	markPrepared(t, manager, sessionID, auth.StreamingProviderChzzk, streaming.ChzzkIngestURL+"/key")

	response, payload := goLive(t, server.URL, sessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with chzzk live (payload %v)", response.StatusCode, payload)
	}
	if failed := goLiveFailedTargets(t, payload); failed[string(auth.StreamingProviderYouTube)] != "streaming_rate_limited" {
		t.Fatalf("failed targets = %v, want youtube streaming_rate_limited", failed)
	}
}

// 요청 한도는 채널(송출 연결) 단위다. 응답 details와 경고 로그에 연결 ID를 싣는다(#505).
func TestRateLimitErrorCarriesStreamingAccount(t *testing.T) {
	var logs bytes.Buffer
	application := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	accountID := uuid.New()
	ctx := auth.WithStreamingAccount(context.Background(), accountID)

	failure := application.prepareError(ctx, streaming.ErrPlatformRateLimited, "session-1", auth.StreamingProviderYouTube)
	if failure.Status != http.StatusTooManyRequests || failure.Code != "streaming_rate_limited" {
		t.Fatalf("error = %d %q, want 429 streaming_rate_limited", failure.Status, failure.Code)
	}
	if failure.Details["account_id"] != accountID.String() {
		t.Fatalf("details = %v, want account_id %s", failure.Details, accountID)
	}
	line := logs.String()
	for _, want := range []string{`"level":"WARN"`, `"msg":"streaming platform rate limited"`, `"session_id":"session-1"`, `"provider":"youtube"`, `"account_id":"` + accountID.String() + `"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log = %s, want %s", line, want)
		}
	}

	// 연결을 고르지 않은 요청(연결이 하나뿐인 경우)은 account_id 없이 응답한다.
	logs.Reset()
	failure = application.prepareError(context.Background(), streaming.ErrPlatformRateLimited, "session-2", auth.StreamingProviderYouTube)
	if _, ok := failure.Details["account_id"]; ok {
		t.Fatalf("details = %v, want no account_id without a selected connection", failure.Details)
	}
	if !strings.Contains(logs.String(), `"session_id":"session-2"`) {
		t.Fatalf("log = %s, want the rate limit logged without a connection", logs.String())
	}
}
