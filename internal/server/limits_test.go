package server

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
	"inno-live-server/internal/usage"

	"github.com/google/uuid"
)

func TestDecideLimits(t *testing.T) {
	h := time.Hour
	idle := 5 * time.Minute
	cases := []struct {
		name    string
		plan    plan.Plan
		onAir   time.Duration
		used    time.Duration
		idle    bool
		idleFor time.Duration
		notices []string
		stop    string
	}{
		{"한도 여유", plan.Beam, h, 10 * h, true, time.Second, nil, ""},
		// Beam 1회 최대 8시간
		{"1회 최대 30분 전", plan.Beam, 7*h + 30*time.Minute, 10 * h, false, 0, []string{noticeBroadcastLimit30m}, ""},
		{"1회 최대 10분 전", plan.Beam, 7*h + 50*time.Minute, 10 * h, false, 0, []string{noticeBroadcastLimit30m, noticeBroadcastLimit10m}, ""},
		{"1회 최대 도달 → 종료", plan.Beam, 8 * h, 10 * h, false, 0,
			[]string{noticeBroadcastLimit30m, noticeBroadcastLimit10m, noticeBroadcastLimitReached}, noticeBroadcastLimitReached},
		// Beam 월 120시간: 80% = 96시간
		{"월 80% 직전", plan.Beam, h, 96*h - time.Second, false, 0, nil, ""},
		{"월 80%", plan.Beam, h, 96 * h, false, 0, []string{noticeMonthlyUsage80}, ""},
		{"유료는 월 100%여도 끊지 않음", plan.Plasma, h, 240 * h, false, 0, []string{noticeMonthlyUsage80, noticeMonthlyUsage100}, ""},
		// Spark 월 5시간·1회 2시간
		{"Spark 월 소진 → 종료", plan.Spark, 30 * time.Minute, 5 * h, false, 0,
			[]string{noticeMonthlyUsage80, noticeMonthlyUsage100, noticeMonthlyLimitReached}, noticeMonthlyLimitReached},
		{"입력 없음 5분 → 종료", plan.Beam, h, h, true, idle, []string{noticeNoInputStopped}, noticeNoInputStopped},
		{"입력 없음 5분 직전", plan.Beam, h, h, true, idle - time.Second, nil, ""},
		{"모두 멈춤이면 입력 없음 판정 안 함", plan.Beam, h, h, false, time.Hour, nil, ""},
		{"Glow는 한도 없음", plan.Glow, 100 * h, 1000 * h, false, 0, nil, ""},
		// 여러 규칙이 겹치면 첫 사유(1회 최대)로 끝낸다
		{"겹치면 1회 최대가 사유", plan.Spark, 2 * h, 5 * h, true, idle,
			[]string{noticeBroadcastLimit30m, noticeBroadcastLimit10m, noticeBroadcastLimitReached, noticeMonthlyUsage80, noticeMonthlyUsage100, noticeMonthlyLimitReached, noticeNoInputStopped}, noticeBroadcastLimitReached},
	}
	for _, test := range cases {
		got := decideLimits(test.plan, test.onAir, test.used, test.idle, test.idleFor, idle)
		if !slices.Equal(got.notices, test.notices) || got.stopReason != test.stop {
			t.Fatalf("%s: notices=%v stop=%q, want %v %q", test.name, got.notices, got.stopReason, test.notices, test.stop)
		}
	}
	// 입력 없음 판정은 설정 0이면 꺼진다.
	if got := decideLimits(plan.Beam, time.Hour, time.Hour, true, 24*time.Hour, 0); got.stopReason != "" {
		t.Fatalf("idle disabled: stop=%q", got.stopReason)
	}
}

type monthlyUsageLedger struct{ used time.Duration }

func (l monthlyUsageLedger) Month(context.Context, uuid.UUID, time.Time, time.Time, time.Time) ([]usage.SessionCharge, error) {
	if l.used == 0 {
		return nil, nil
	}
	return []usage.SessionCharge{{SessionID: uuid.New(), Charged: l.used, OnAir: l.used}}, nil
}

// 월 방송 시간을 다 쓰면 다음 방송 준비가 플랫폼 호출 전에 막힌다 — 유료도 같다.
func TestPrepareBlockedWhenMonthlyLimitExhausted(t *testing.T) {
	for _, test := range []struct {
		plan      plan.Plan
		used      time.Duration
		wantCode  string
		wantCalls int
	}{
		{plan.Spark, 5 * time.Hour, "monthly_limit_exhausted", 0},
		{plan.Beam, 120 * time.Hour, "monthly_limit_exhausted", 0},
		{plan.Spark, 4 * time.Hour, "", 1},
	} {
		t.Run(string(test.plan), func(t *testing.T) {
			youtube := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected}
			server, manager, application := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
				auth.StreamingProviderYouTube: youtube,
			})
			manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return test.plan, nil })
			application.usageLedger = monthlyUsageLedger{used: test.used}
			live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
			if err != nil {
				t.Fatal(err)
			}
			putBroadcast(t, server.URL, live.ID, ownerToken, `{"made_for_kids":false}`)
			response, payload := prepareStream(t, server.URL, live.ID, ownerToken, `{}`)
			if test.wantCode != "" && (response.StatusCode != http.StatusForbidden || streamErrorCode(payload) != test.wantCode) {
				t.Fatalf("status=%d code=%q, want 403 %s", response.StatusCode, streamErrorCode(payload), test.wantCode)
			}
			if youtube.prepareCalls != test.wantCalls {
				t.Fatalf("platform prepare calls = %d, want %d", youtube.prepareCalls, test.wantCalls)
			}
			if test.wantCode != "" && live.BusyTargetCount() != 0 {
				t.Fatal("rejected prepare must release its preparation")
			}
		})
	}
}

type sessionOnAirLedger struct {
	sessionID uuid.UUID
	onAir     time.Duration
}

func (l *sessionOnAirLedger) Month(context.Context, uuid.UUID, time.Time, time.Time, time.Time) ([]usage.SessionCharge, error) {
	return []usage.SessionCharge{{SessionID: l.sessionID, OnAir: l.onAir, Charged: l.onAir}}, nil
}

// 1회 최대에 닿아 끝난 세션에서 다시 준비하면, 잠깐 송출됐다가 다음 점검에서
// 또 끊긴다 — 준비 단계에서 막아야 한다.
func TestPrepareBlockedWhenSessionReachedBroadcastLimit(t *testing.T) {
	youtube := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected}
	server, manager, application := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
	})
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Spark, nil })
	live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Spark 1회 최대 2시간을 이 세션이 이미 썼다(월 사용량은 2시간 < 5시간).
	application.usageLedger = &sessionOnAirLedger{sessionID: uuid.MustParse(live.ID), onAir: 2 * time.Hour}
	putBroadcast(t, server.URL, live.ID, ownerToken, `{"made_for_kids":false}`)
	response, payload := prepareStream(t, server.URL, live.ID, ownerToken, `{}`)
	if response.StatusCode != http.StatusForbidden || streamErrorCode(payload) != "broadcast_limit_reached" {
		t.Fatalf("status=%d code=%q, want 403 broadcast_limit_reached", response.StatusCode, streamErrorCode(payload))
	}
	if youtube.prepareCalls != 0 || live.BusyTargetCount() != 0 {
		t.Fatalf("prepare calls=%d busy=%d, want no platform call and released preparation", youtube.prepareCalls, live.BusyTargetCount())
	}
}

func TestBroadcastRemaining(t *testing.T) {
	h := time.Hour
	cases := []struct {
		name  string
		plan  plan.Plan
		onAir time.Duration
		used  time.Duration
		units int
		want  time.Duration // -1 = nil
	}{
		// Plasma 월 240h 중 198h 사용 → 잔여 42h. FHD 동시(3배)면 14h, 1회 잔여(12h−1h=11h)가 더 작다.
		{"1회 잔여가 더 작음", plan.Plasma, h, 198 * h, 3, 11 * h},
		// 잔여 42h를 FHD 동시로 쓰면 14h. 방송 막 시작(1회 잔여 12h)이면 12h.
		{"1회 잔여가 제한", plan.Plasma, 0, 198 * h, 3, 12 * h},
		// Beam 잔여 5h, FHD(2배) → 2.5h. 1회 잔여 7h보다 작다.
		{"월 잔여 ÷ 배수가 제한", plan.Beam, h, 115 * h, 2, 150 * time.Minute},
		{"다 쓰면 0(음수 아님)", plan.Spark, 30 * time.Minute, 6 * h, 1, 0},
		{"Glow는 무제한", plan.Glow, 10 * h, 100 * h, 1, -1},
	}
	for _, test := range cases {
		got := broadcastRemaining(test.plan, test.onAir, test.used, test.units)
		if test.want < 0 {
			if got != nil {
				t.Fatalf("%s: got %v, want nil", test.name, *got)
			}
			continue
		}
		if got == nil || *got != test.want {
			t.Fatalf("%s: got %v, want %v", test.name, got, test.want)
		}
	}
}
