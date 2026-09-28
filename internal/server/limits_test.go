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
