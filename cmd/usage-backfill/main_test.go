package main

import (
	"strings"
	"testing"
	"time"
)

const sampleLog = `-- Journal begins --
2026/08/05 13:59:11 inno-live-server/internal/auth/email_auth.go:358 record not found
{"time":"2026-09-01T10:00:00Z","level":"INFO","msg":"created live session","session_id":"s1","user_id":"a1"}
{"time":"2026-09-01T10:01:00Z","level":"INFO","msg":"RTMP egress started","session_id":"s1","provider":"youtube","url":"rtmps://a.rtmps.youtube.com/live2/****"}
{"time":"2026-09-01T10:02:00Z","level":"INFO","msg":"platform broadcast is live","session_id":"s1"}
{"time":"2026-09-01T10:10:00Z","level":"INFO","msg":"RTMP egress paused","session_id":"s1"}
{"time":"2026-09-01T10:12:00Z","level":"INFO","msg":"RTMP egress resumed","session_id":"s1"}
{"time":"2026-09-01T10:30:00Z","level":"INFO","msg":"RTMP egress stopped","session_id":"s1","provider":"youtube","reason":"user_requested"}
{"time":"2026-09-01T10:31:00Z","level":"INFO","msg":"closed live session","session_id":"s1","reason":"peer_connection_closed"}
{"time":"2026-09-02T09:00:00Z","level":"INFO","msg":"created live session","session_id":"s2","user_id":"00000000-0000-0000-0000-000000000000"}
{"time":"2026-09-02T09:01:00Z","level":"INFO","msg":"RTMP egress started","session_id":"s2"}
{"time":"2026-09-02T09:20:00Z","level":"INFO","msg":"closed live session","session_id":"s2","reason":"application_shutdown"}
{"time":"2026-09-03T09:00:00Z","level":"INFO","msg":"created live session","session_id":"s3","user_id":"a1"}
{"time":"2026-09-03T09:05:00Z","level":"INFO","msg":"RTMP egress started","session_id":"s3","url":"rtmp://global-rtmp.lip2.navercorp.com:8080/relay/****"}
{"time":"2026-09-10T00:00:00Z","level":"INFO","msg":"created live session","session_id":"after","user_id":"a1"}
`

// TestParseRebuildsLifecycle: 로그 사건을 세션·송출 행으로 잇고, 종료 로그가
// 없는 송출은 세션 종료로 추정 마감하며, 종료가 아예 없는 행은 열린 채 둔다.
func TestParseRebuildsLifecycle(t *testing.T) {
	until := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	sessions, broadcasts, err := parse(strings.NewReader(sampleLog), until)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 || sessions[0].ID != "s1" || sessions[2].ID != "s3" {
		t.Fatalf("sessions = %+v, want s1..s3 in order (after -until skipped)", sessions)
	}
	if len(broadcasts) != 3 {
		t.Fatalf("broadcasts = %d, want 3", len(broadcasts))
	}

	first := broadcasts[0]
	if first.LiveAt == nil || first.EndReason != "user_requested" || first.Estimated || first.PausedSeconds != 120 {
		t.Fatalf("s1 broadcast = %+v", first)
	}
	second := broadcasts[1]
	if second.Provider != "youtube" || second.EndedAt == nil || !second.Estimated || second.EndReason != "session_closed" {
		t.Fatalf("s2 broadcast = %+v, want provider default and estimated end", second)
	}
	third := broadcasts[2]
	if third.Provider != "chzzk" {
		t.Fatalf("s3 provider = %q, want chzzk inferred from ingest host", third.Provider)
	}
	if third.EndedAt != nil || sessions[2].EndedAt != nil {
		t.Fatalf("s3 should stay open for unclean_shutdown: %+v %+v", third, sessions[2])
	}
}

// TestWriteSQLGuestsAndUnclean: 게스트는 user_id 없이, 종료가 없는 행은
// unclean_shutdown으로, 송출은 백필 세션에만 붙는 SQL이 나온다.
func TestWriteSQLGuestsAndUnclean(t *testing.T) {
	sessions, broadcasts, err := parse(strings.NewReader(sampleLog), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	writeSQL(&out, sessions, broadcasts)
	sql := out.String()
	for _, want := range []string{
		"VALUES ('s2', NULL, true,",
		"(SELECT id FROM users WHERE id = 'a1'), false",
		"'unclean_shutdown', 'backfill'",
		"AND source = 'backfill') ON CONFLICT DO NOTHING",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL missing %q:\n%s", want, sql)
		}
	}
	if !strings.HasPrefix(sql, "BEGIN;") || !strings.HasSuffix(sql, "COMMIT;\n") {
		t.Fatal("SQL is not wrapped in a transaction")
	}
}
