package auth

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
)

// ErrDemoLockdown은 시연 모드(#490)가 관리자가 아닌 계정의 토큰 발급·인증이나
// 새 계정 생성을 거절할 때 쓴다.
var ErrDemoLockdown = errors.New("demo lockdown: administrators only")

// DemoLockdown은 시연 중 서버를 관리자 계정에만 여는 스위치다. nil이면 꺼진
// 것이고 모든 검사를 통과시킨다 — 꺼진 배포의 동작은 종전과 같다.
type DemoLockdown struct {
	allowed map[uuid.UUID]struct{}
}

// NewDemoLockdown은 allowed에 있는 사용자만 통과시키는 시연 모드를 만든다.
func NewDemoLockdown(allowed []uuid.UUID) *DemoLockdown {
	lockdown := &DemoLockdown{allowed: make(map[uuid.UUID]struct{}, len(allowed))}
	for _, id := range allowed {
		lockdown.allowed[id] = struct{}{}
	}
	return lockdown
}

// Enabled는 시연 모드가 켜져 있는지 답한다.
func (l *DemoLockdown) Enabled() bool {
	return l != nil
}

// Admit은 userID가 시연 중에도 쓸 수 있는 계정인지 본다. 꺼져 있으면 항상 nil이다.
func (l *DemoLockdown) Admit(userID uuid.UUID) error {
	if l == nil {
		return nil
	}
	if _, ok := l.allowed[userID]; ok && userID != uuid.Nil {
		return nil
	}
	return ErrDemoLockdown
}

// 거절은 503으로 답한다. 401로 답하면 앱이 저장한 토큰을 지워 시연이 끝난 뒤
// 사용자가 다시 로그인해야 하고, 403은 영구 거절처럼 읽힌다.
const (
	demoLockdownStatus  = http.StatusServiceUnavailable
	demoLockdownCode    = "demo_lockdown"
	demoLockdownMessage = "InnoLive is temporarily limited to administrators for a demonstration."
)

func (h *tokenHTTPHandler) writeDemoLockdown(w http.ResponseWriter, r *http.Request) {
	h.writeError(w, r, demoLockdownStatus, demoLockdownCode, demoLockdownMessage)
}

// demoLockdownClosed는 시연 모드에서 가입처럼 계정을 만들거나 바꾸는 경로를 닫는다.
// 요청 본문에 사용자가 아직 정해지지 않아 관리자 여부를 가릴 수 없으므로 관리자도
// 막는다. CORS preflight는 그대로 넘겨 브라우저가 거절 본문을 읽게 한다.
func (h *tokenHTTPHandler) demoLockdownClosed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || !h.service.demoLockdown().Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		h.writeDemoLockdown(w, r)
	})
}
