package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// endCheckingProvider는 플랫폼 방송 종료 여부를 돌려주는 스텁이다.
type endCheckingProvider struct {
	*stubStreamingProvider
	mu     *sync.Mutex
	ended  bool
	checks *int
	quota  *streaming.QuotaMeter
}

func (p endCheckingProvider) Quota() *streaming.QuotaMeter { return p.quota }

func (p endCheckingProvider) BroadcastEnded(context.Context, uuid.UUID, string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.checks++
	return p.ended, nil
}

func platformEndFixture(t *testing.T, ended bool, quotaWrites ...int) (*Server, *session.Session, *int) {
	t.Helper()
	checks := 0
	meter := streaming.NewQuotaMeter()
	for _, writes := range quotaWrites {
		for i := 0; i < writes; i++ {
			meter.Record(http.MethodPost)
		}
	}
	provider := endCheckingProvider{stubStreamingProvider: &stubStreamingProvider{}, mu: &sync.Mutex{}, ended: ended, checks: &checks, quota: meter}
	_, manager, application := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	live, _, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(live.ID, "youtube"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(live.ID, session.PlatformBroadcast{Provider: "youtube", BroadcastID: "yt-1"}, "youtube"); err != nil {
		t.Fatal(err)
	}
	return application, live, &checks
}

// 진행 중인 방송은 주기마다 한 번만 묻고, 건드리지 않는다(#360).
func TestPlatformEndCheckRespectsInterval(t *testing.T) {
	application, live, checks := platformEndFixture(t, false)
	now := time.Now()
	application.stopPlatformEndedTargets(context.Background(), live, []string{"youtube"}, now)
	application.stopPlatformEndedTargets(context.Background(), live, []string{"youtube"}, now.Add(time.Minute))
	if *checks != 1 {
		t.Fatalf("checks within the interval = %d, want 1", *checks)
	}
	application.stopPlatformEndedTargets(context.Background(), live, []string{"youtube"}, now.Add(platformEndCheckInterval))
	if *checks != 2 {
		t.Fatalf("checks after the interval = %d, want 2", *checks)
	}
	if broadcast, _ := live.PlatformBroadcast("youtube"); broadcast.BroadcastID != "yt-1" {
		t.Fatal("a running broadcast must not be stopped")
	}
}

// 플랫폼에서 끝난 방송은 송출을 멈추고 알림을 남긴다.
func TestPlatformEndedBroadcastStopsTarget(t *testing.T) {
	application, live, _ := platformEndFixture(t, true)
	application.stopPlatformEndedTargets(context.Background(), live, []string{"youtube"}, time.Now())
	if live.BusyTargetCount() != 0 {
		t.Fatal("the ended target must be stopped")
	}
	found := false
	for _, notice := range live.Response().Notices {
		if notice.Code == noticePlatformEnded {
			found = true
		}
	}
	if !found {
		t.Fatalf("notices = %+v, want %s", live.Response().Notices, noticePlatformEnded)
	}
}

// 쿼터를 절반 넘게 쓰면 10분마다, 80%를 넘으면 쉰다(#366).
func TestPlatformEndCheckSlowsDownWithQuota(t *testing.T) {
	application, live, checks := platformEndFixture(t, false, 100) // 5,000 = 50%
	now := time.Now()
	application.stopPlatformEndedTargets(context.Background(), live, []string{"youtube"}, now)
	application.stopPlatformEndedTargets(context.Background(), live, []string{"youtube"}, now.Add(platformEndCheckInterval))
	if *checks != 1 {
		t.Fatalf("checks at 5 minutes with half the quota used = %d, want 1", *checks)
	}
	application.stopPlatformEndedTargets(context.Background(), live, []string{"youtube"}, now.Add(platformEndCheckSlowInterval))
	if *checks != 2 {
		t.Fatalf("checks at 10 minutes = %d, want 2", *checks)
	}

	resting, restingLive, restingChecks := platformEndFixture(t, false, 160) // 8,000 = 80%
	resting.stopPlatformEndedTargets(context.Background(), restingLive, []string{"youtube"}, now)
	if *restingChecks != 0 {
		t.Fatalf("checks with low quota = %d, want 0", *restingChecks)
	}
}

// 쿼터가 모자라면 유튜브로 방송 중인 세션에만 알림을 한 번 남긴다(#366).
func TestNoticeYouTubeQuotaLow(t *testing.T) {
	application, live, _ := platformEndFixture(t, false, 160)
	application.noticeYouTubeQuotaLowFor(live, []string{"chzzk"}, time.Now())
	application.noticeYouTubeQuotaLowFor(live, []string{"youtube"}, time.Now())
	application.noticeYouTubeQuotaLowFor(live, []string{"youtube"}, time.Now())
	count := 0
	for _, notice := range live.Response().Notices {
		if notice.Code == noticeYouTubeQuotaLow {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("quota notices = %d, want 1 (youtube only, once)", count)
	}
}
