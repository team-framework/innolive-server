package usage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresLocalTakesCountOnceInStartMonth(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	owner, other := createTestUser(t, db), createTestUser(t, db)
	ledger := NewLedger(db)
	ctx := context.Background()
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, LedgerLocation).UTC() }
	take := func(user uuid.UUID, start time.Time, seconds int64) LocalTake {
		return LocalTake{TakeID: uuid.New(), UserID: &user, DeviceID: "device-1", StartedAt: start,
			EndedAt: start.Add(time.Duration(seconds) * time.Second), DurationSeconds: seconds, ReceivedAt: at(31, 23)}
	}
	october := take(owner, at(10, 12), 600)
	// 10월 31일 23:50 KST에 시작해 11월로 넘어간 촬영은 10월에 센다.
	crossing := take(owner, at(31, 23).Add(50*time.Minute), 1200)
	november := take(owner, at(31, 23).Add(2*time.Hour), 60)
	foreign := take(other, at(10, 12), 900)

	for attempt := 0; attempt < 2; attempt++ {
		if err := ledger.RecordLocalTakes(ctx, []LocalTake{october, crossing, november, foreign}); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	retry := take(other, at(11, 12), 5)
	retry.TakeID = october.TakeID
	if err := ledger.RecordLocalTakes(ctx, []LocalTake{retry}); err != nil {
		t.Fatal(err)
	}
	var rows int64
	if err := db.Model(&LocalTake{}).Count(&rows).Error; err != nil || rows != 4 {
		t.Fatalf("rows = %d, %v, want 4 after replays", rows, err)
	}

	from, to := MonthRange(at(10, 12))
	charges, err := ledger.Month(ctx, owner, from, to, at(31, 23))
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 2 || charges[0].SessionID != october.TakeID || charges[1].SessionID != crossing.TakeID {
		t.Fatalf("october charges = %+v, want the two owner takes started in October", charges)
	}
	for _, charge := range charges {
		if !charge.Local || !charge.OnDevice || charge.Charged != charge.OnAir {
			t.Fatalf("charge = %+v, want local on-device time charged 1x", charge)
		}
	}
	if charges[1].Charged != 20*time.Minute {
		t.Fatalf("crossing charge = %v, want 20m", charges[1].Charged)
	}
}
