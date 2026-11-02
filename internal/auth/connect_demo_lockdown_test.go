package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// 플랫폼 연결 핸들러는 인증 미들웨어 대신 토큰을 직접 검증한다. 시연 모드(#490)
// 확인을 빠뜨리면 막아 둔 계정이 시연 중에 송출 계정을 연결할 수 있다(P1-19).
func TestPlatformConnectHandlersHonorDemoLockdown(t *testing.T) {
	service := testTokenService(newMemoryRefreshStore())
	member := uuid.New()
	token, err := service.issueAccessToken(member, uuid.New(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	service.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{uuid.New()}))
	h := &tokenHTTPHandler{service: service, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for name, handle := range map[string]http.HandlerFunc{
		"youtube connect":    h.handleYouTubeConnect,
		"chzzk connect":      h.handleChzzkConnect,
		"streaming accounts": h.handleListStreamingAccounts,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handle(response, request)
			assertDemoLockdownResponse(t, response)
		})
	}
}
