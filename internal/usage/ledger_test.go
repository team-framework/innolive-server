package usage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestChargeSession(t *testing.T) {
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, LedgerLocation)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	end := func(minutes int) *time.Time { value := at(minutes); return &value }
	from, to := MonthRange(base)
	now := at(600)
	hours := func(h float64) time.Duration { return time.Duration(h * float64(time.Hour)) }

	cases := []struct {
		name    string
		fhd     bool
		targets []TargetTimeline
		onAir   time.Duration
		charged time.Duration
	}{
		{"720p 한 곳 1시간 → 1시간", false,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(60)}}, hours(1), hours(1)},
		{"FHD 한 곳 1시간 → 2시간", true,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(60)}}, hours(1), hours(2)},
		{"720p 동시 1시간 → 2시간", false,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(60)}, {LiveAt: at(0), EndedAt: end(60)}}, hours(1), hours(2)},
		{"FHD 동시 1시간 → 3시간", true,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(60)}, {LiveAt: at(0), EndedAt: end(60)}}, hours(1), hours(3)},
		// 한쪽이 30분에 끝나면 그 뒤는 FHD 단독(2배): 0.5×3 + 0.5×2 = 2.5
		{"FHD 동시 중 한쪽이 먼저 끝남", true,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(30)}, {LiveAt: at(0), EndedAt: end(60)}}, hours(1), hours(2.5)},
		// 한쪽만 20분 멈춤: 40분×3 + 20분×2
		{"FHD 동시 중 한쪽만 멈춤", true,
			[]TargetTimeline{
				{LiveAt: at(0), EndedAt: end(60), Pauses: []PauseSpan{{PausedAt: at(20), ResumedAt: end(40)}}},
				{LiveAt: at(0), EndedAt: end(60)},
			}, hours(1), 40*time.Minute*3 + 20*time.Minute*2},
		{"일시정지 구간은 차감하지 않음", false,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(60), Pauses: []PauseSpan{{PausedAt: at(10), ResumedAt: end(40)}}}}, 30 * time.Minute, 30 * time.Minute},
		{"멈춘 채 끝남(열린 구간)", false,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(60), Pauses: []PauseSpan{{PausedAt: at(45)}}}}, 45 * time.Minute, 45 * time.Minute},
		{"둘 다 멈추면 0", true,
			[]TargetTimeline{
				{LiveAt: at(0), EndedAt: end(60), Pauses: []PauseSpan{{PausedAt: at(0), ResumedAt: end(60)}}},
				{LiveAt: at(0), EndedAt: end(60), Pauses: []PauseSpan{{PausedAt: at(0)}}},
			}, 0, 0},
		{"진행 중 송출은 지금까지", false,
			[]TargetTimeline{{LiveAt: at(540)}}, hours(1), hours(1)},
		{"진행 중 멈춤은 지금까지 빠짐", false,
			[]TargetTimeline{{LiveAt: at(480), Pauses: []PauseSpan{{PausedAt: at(540)}}}}, hours(1), hours(1)},
		{"비정상 종료(종료 기록 없음)는 0", false,
			[]TargetTimeline{{LiveAt: at(0), Unclean: true}}, 0, 0},
		{"구간 기록 이전 행은 합계를 끝에서 뺀다", false,
			[]TargetTimeline{{LiveAt: at(0), EndedAt: end(60), LegacyPausedSeconds: 600}}, 50 * time.Minute, 50 * time.Minute},
	}
	for _, test := range cases {
		onAir, charged := ChargeSession(test.fhd, test.targets, from, to, now)
		if onAir != test.onAir || charged != test.charged {
			t.Fatalf("%s: onAir=%v charged=%v, want %v / %v", test.name, onAir, charged, test.onAir, test.charged)
		}
	}
}

func TestChargeSessionSplitsAtMonthBoundary(t *testing.T) {
	// 9월 30일 23:00 KST ~ 10월 1일 01:00 KST, FHD 한 곳 → 9월 1시간(2) · 10월 1시간(2)
	liveAt := time.Date(2026, 9, 30, 23, 0, 0, 0, LedgerLocation)
	ended := liveAt.Add(2 * time.Hour)
	targets := []TargetTimeline{{LiveAt: liveAt, EndedAt: &ended}}
	now := ended.Add(time.Hour)

	sepFrom, sepTo := MonthRange(liveAt)
	octFrom, octTo := MonthRange(ended)
	if !sepTo.Equal(octFrom) || octFrom.Day() != 1 || octFrom.Hour() != 0 {
		t.Fatalf("month ranges: sep [%v, %v) oct [%v, %v)", sepFrom, sepTo, octFrom, octTo)
	}
	_, september := ChargeSession(true, targets, sepFrom, sepTo, now)
	_, october := ChargeSession(true, targets, octFrom, octTo, now)
	if september != 2*time.Hour || october != 2*time.Hour {
		t.Fatalf("september=%v october=%v, want 2h each", september, october)
	}
}

func TestMonthRangeUsesKST(t *testing.T) {
	// UTC로는 9월 30일 15:30이지만 KST로는 10월 1일 00:30이다.
	from, to := MonthRange(time.Date(2026, 9, 30, 15, 30, 0, 0, time.UTC))
	if from.Month() != time.October || !to.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, LedgerLocation)) {
		t.Fatalf("MonthRange = [%v, %v), want October KST", from, to)
	}
}

func TestPostgresLedgerMonth(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	owner := createTestUser(t, db)
	other := createTestUser(t, db)
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, LedgerLocation)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute).UTC() }
	ptr := func(value time.Time) *time.Time { return &value }
	fhd, hd := "fhd", "720p"
	reason := "user_requested"

	mustCreate := func(value any) {
		t.Helper()
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	// owner: FHD 동시 1시간(치지직 30분 먼저 종료, 유튜브 10분 멈춤)
	fhdSession := Session{ID: uuid.New(), UserID: &owner, StartedAt: at(-5), EndedAt: ptr(at(65)), Resolution: &fhd, Source: SourceLive}
	mustCreate(&fhdSession)
	youtube := Broadcast{ID: uuid.New(), SessionID: fhdSession.ID, Provider: "youtube", StartedAt: at(-1), LiveAt: ptr(at(0)), EndedAt: ptr(at(60)), EndReason: &reason, Source: SourceLive}
	chzzk := Broadcast{ID: uuid.New(), SessionID: fhdSession.ID, Provider: "chzzk", StartedAt: at(0), LiveAt: ptr(at(0)), EndedAt: ptr(at(30)), EndReason: &reason, Source: SourceLive}
	mustCreate(&youtube)
	mustCreate(&chzzk)
	mustCreate(&Pause{ID: uuid.New(), BroadcastID: youtube.ID, PausedAt: at(40), ResumedAt: ptr(at(50))})
	// owner: 준비만 하고 라이브로 가지 않은 세션 → 내역에서 빠진다
	idle := Session{ID: uuid.New(), UserID: &owner, StartedAt: at(100), EndedAt: ptr(at(110)), Resolution: &hd, Source: SourceLive}
	mustCreate(&idle)
	mustCreate(&Broadcast{ID: uuid.New(), SessionID: idle.ID, Provider: "youtube", StartedAt: at(101), EndedAt: ptr(at(109)), EndReason: &reason, Source: SourceLive})
	// other: 섞이면 안 된다
	otherSession := Session{ID: uuid.New(), UserID: &other, StartedAt: at(0), EndedAt: ptr(at(60)), Resolution: &hd, Source: SourceLive}
	mustCreate(&otherSession)
	mustCreate(&Broadcast{ID: uuid.New(), SessionID: otherSession.ID, Provider: "youtube", StartedAt: at(0), LiveAt: ptr(at(0)), EndedAt: ptr(at(60)), EndReason: &reason, Source: SourceLive})

	from, to := MonthRange(base)
	charges, err := NewLedger(db).Month(context.Background(), owner, from, to, at(600))
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 1 || charges[0].SessionID != fhdSession.ID {
		t.Fatalf("charges = %+v, want only the FHD session", charges)
	}
	// 0~30분 FHD 동시(3) · 30~40 FHD 단독(2) · 40~50 멈춤 · 50~60 FHD 단독(2) = 90+20+20 = 130분
	got := charges[0]
	if got.OnAir != 50*time.Minute || got.Charged != 130*time.Minute || got.Resolution != "fhd" ||
		len(got.Providers) != 2 || got.Providers[0] != "chzzk" || got.Providers[1] != "youtube" {
		t.Fatalf("charge = %+v", got)
	}
	// 다른 달에는 없다
	octFrom, octTo := MonthRange(to)
	if charges, err := NewLedger(db).Month(context.Background(), owner, octFrom, octTo, at(600)); err != nil || len(charges) != 0 {
		t.Fatalf("october charges = %+v, %v", charges, err)
	}
}
