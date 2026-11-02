package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// ctxRecordingProvider는 준비 왕복이 끝날 때 컨텍스트가 살아 있었는지 남긴다.
type ctxRecordingProvider struct {
	*stubStreamingProvider
	prepareCtxErr chan error
}

func (p *ctxRecordingProvider) Prepare(ctx context.Context, userID uuid.UUID, options streaming.PrepareOptions) (streaming.PreparedBroadcast, error) {
	prepared, err := p.stubStreamingProvider.Prepare(ctx, userID, options)
	p.prepareCtxErr <- ctx.Err()
	return prepared, err
}

// 플랫폼 방송을 만드는 왕복은 여러 호출로 이뤄진다. 클라이언트가 중간에 끊었다고
// 왕복을 취소하면 만들다 만 방송이 플랫폼에 고아로 남는다(P1-10).
func TestPrepareKeepsPlatformCallsAliveWhenClientDisconnects(t *testing.T) {
	provider := &ctxRecordingProvider{
		stubStreamingProvider: &stubStreamingProvider{
			prepared:       streaming.PreparedBroadcast{Provider: auth.StreamingProviderYouTube, IngestURL: "rtmps://a.rtmps.youtube.com/live2/secret"},
			prepareEntered: make(chan struct{}),
			prepareRelease: make(chan struct{}),
		},
		prepareCtxErr: make(chan error, 1),
	}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	if response, payload := putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"title":"방송","made_for_kids":false}`); response.StatusCode != http.StatusOK {
		t.Fatalf("put broadcast status = %d (payload %v)", response.StatusCode, payload)
	}

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/sessions/"+created.SessionID+"/stream/prepare", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-Owner-Token", ownerToken)
	go func() {
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-provider.prepareEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("prepare did not reach the platform")
	}
	cancel()
	time.Sleep(100 * time.Millisecond) // 서버가 연결 끊김을 알아챌 시간
	close(provider.prepareRelease)

	select {
	case err := <-provider.prepareCtxErr:
		if err != nil {
			t.Fatalf("platform prepare context = %v after the client left, want it kept alive", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prepare did not finish")
	}
}
