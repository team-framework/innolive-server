package usage

import (
	"context"
	"testing"
	"time"

	"inno-live-server/internal/session"

	"github.com/google/uuid"
)

// 송출 종료 이벤트가 유실되면 송출 행이 열린 채 남는다. 세션 종료가 기록됐다면
// 송출도 그때 끝났으므로 그 시각에서 자른다. 그렇지 않으면 원장이 지금까지 계속
// 차감한다(P0-10).
func TestPostgresLedgerCutsLostBroadcastEndAtSessionEnd(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	owner := createTestUser(t, db)
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, LedgerLocation)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute).UTC() }
	ptr := func(value time.Time) *time.Time { return &value }
	hd := "720p"
	ended := "peer_connection_closed"

	sessionRow := Session{ID: uuid.New(), UserID: &owner, StartedAt: at(-1), EndedAt: ptr(at(30)), EndReason: &ended, Resolution: &hd, Source: SourceLive}
	if err := db.Create(&sessionRow).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Broadcast{ID: uuid.New(), SessionID: sessionRow.ID, Provider: "youtube", StartedAt: at(0), LiveAt: ptr(at(0)), Source: SourceLive}).Error; err != nil {
		t.Fatal(err)
	}

	from, to := MonthRange(base)
	charges, err := NewLedger(db).Month(context.Background(), owner, from, to, at(600))
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 1 || charges[0].Charged != 30*time.Minute {
		t.Fatalf("charges = %+v, want 30m cut at the session end", charges)
	}
}

// 프로세스가 죽어 종료 기록 없이 unclean으로 마감된 온디바이스 송출은 앱이 보낸
// 마지막 하트비트까지 송출한 것으로 본다. 하트비트가 없는 서버 송출은 끝난 시각을
// 모르므로 지금처럼 차감하지 않는다.
func TestPostgresLedgerEstimatesUncleanOnDeviceBroadcastFromHeartbeat(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	owner := createTestUser(t, db)
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, LedgerLocation)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute).UTC() }
	ptr := func(value time.Time) *time.Time { return &value }
	hd := "720p"
	onDevice, server := session.AIProcessingOnDevice, session.AIProcessingServer
	unclean := ReasonUncleanShutdown

	device := Session{ID: uuid.New(), UserID: &owner, StartedAt: at(-1), EndReason: &unclean, Resolution: &hd, AIProcessing: &onDevice, Source: SourceLive}
	serverSession := Session{ID: uuid.New(), UserID: &owner, StartedAt: at(99), EndReason: &unclean, Resolution: &hd, AIProcessing: &server, Source: SourceLive}
	for _, row := range []*Session{&device, &serverSession} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []Broadcast{
		{ID: uuid.New(), SessionID: device.ID, Provider: "youtube", StartedAt: at(0), LiveAt: ptr(at(0)), EndReason: &unclean, Source: SourceLive},
		{ID: uuid.New(), SessionID: serverSession.ID, Provider: "youtube", StartedAt: at(100), LiveAt: ptr(at(100)), EndReason: &unclean, Source: SourceLive},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	store := NewHeartbeatStore(db)
	for _, minute := range []int{5, 20} {
		if err := store.SaveHeartbeat(context.Background(), Heartbeat{SessionID: device.ID, ReceivedAt: at(minute), SentAt: at(minute), Platform: "ios", Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
	}

	from, to := MonthRange(base)
	charges, err := NewLedger(db).Month(context.Background(), owner, from, to, at(600))
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 1 || charges[0].SessionID != device.ID || charges[0].Charged != 20*time.Minute {
		t.Fatalf("charges = %+v, want only the on-device session charged 20m", charges)
	}
}
