package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// 인증 저장소가 실패한 것은 자격 정보가 틀린 것과 다르다. 401로 답하면 앱이
// 다시 로그인을 시키고, 기한 없는 컨텍스트로 조회하면 DB가 멈춘 동안 시그널링이
// 같이 멈춘다(P1-30).
func TestSignalingAuthDistinguishesUnavailableStoreAndBoundsLookup(t *testing.T) {
	requireUser, _, _, _, _ := testRequireUser(t)
	var hadDeadline bool
	failing := func(ctx context.Context, _ string) (uuid.UUID, error) {
		_, hadDeadline = ctx.Deadline()
		return uuid.Nil, errors.New("database is unavailable")
	}
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, failing)
	defer manager.CloseAll()
	live, ownerToken, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}

	release, apiErr := application.verifySignalingSession(context.Background(), live.ID, ownerToken, "access-token", "")
	if release != nil {
		release()
	}
	if apiErr == nil || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "auth_unavailable" {
		t.Fatalf("signaling auth with a failing store = %+v, want 503 auth_unavailable", apiErr)
	}
	if !hadDeadline {
		t.Fatal("user lookup ran without a deadline")
	}
}
