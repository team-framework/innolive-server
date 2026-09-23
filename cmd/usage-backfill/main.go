// usage-backfill은 실사용 기록(#266) 도입 이전의 journald 로그에서
// usage_sessions/usage_broadcasts 행을 복원하는 SQL을 만든다. 일회성 도구다.
//
//	ssh <host> 'journalctl -u innolive-server -o cat' \
//	  | go run ./cmd/usage-backfill -until 2026-09-24T00:00:00Z > backfill.sql
//
// -until에는 기록 기능이 배포된 시각을 준다. 그 이후 생성된 세션은 서버가 직접
// 기록하므로 건너뛴다. 생성된 SQL은 멱등(ON CONFLICT DO NOTHING)이다.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 백필 송출 id를 결정적으로 만든다 — 같은 로그로 다시 돌려도 같은 행이다.
var broadcastNamespace = uuid.MustParse("6f1c1f7e-7a3b-4a53-9a55-1d2f3b0c8e66")

type logLine struct {
	Time       time.Time `json:"time"`
	Msg        string    `json:"msg"`
	SessionID  string    `json:"session_id"`
	UserID     string    `json:"user_id"`
	Provider   string    `json:"provider"`
	Reason     string    `json:"reason"`
	StopReason string    `json:"stop_reason"`
	// 송출 시작 로그의 마스킹된 출력 URL. provider 필드가 없던 시절의 플랫폼
	// 판별에만 쓰고 출력하지 않는다.
	URL string `json:"url"`
}

type sessionRow struct {
	ID        string
	UserID    string
	StartedAt time.Time
	EndedAt   *time.Time
	EndReason string
}

type broadcastRow struct {
	ID            string
	SessionID     string
	Provider      string
	StartedAt     time.Time
	LiveAt        *time.Time
	EndedAt       *time.Time
	EndReason     string
	PausedSeconds float64
	Estimated     bool
	pausedAt      *time.Time
}

func main() {
	untilFlag := flag.String("until", "", "RFC3339; 이 시각 이후 생성된 세션은 건너뛴다")
	flag.Parse()
	var until time.Time
	if *untilFlag != "" {
		parsed, err := time.Parse(time.RFC3339, *untilFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid -until:", err)
			os.Exit(2)
		}
		until = parsed
	}
	sessions, broadcasts, err := parse(os.Stdin, until)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	writeSQL(os.Stdout, sessions, broadcasts)
	estimated := 0
	for _, b := range broadcasts {
		if b.Estimated {
			estimated++
		}
	}
	fmt.Fprintf(os.Stderr, "sessions=%d broadcasts=%d estimated_end=%d\n", len(sessions), len(broadcasts), estimated)
}

func parse(r io.Reader, until time.Time) ([]*sessionRow, []*broadcastRow, error) {
	sessions := map[string]*sessionRow{}
	var broadcasts []*broadcastRow
	open := map[string][]*broadcastRow{} // session_id → 아직 끝나지 않은 송출

	endOpen := func(sessionID string, match func(*broadcastRow) bool, at time.Time, reason string, estimated bool) {
		remaining := open[sessionID][:0]
		for _, b := range open[sessionID] {
			if !match(b) {
				remaining = append(remaining, b)
				continue
			}
			end := at
			b.EndedAt, b.EndReason, b.Estimated = &end, reason, estimated
			if b.pausedAt != nil {
				b.PausedSeconds += at.Sub(*b.pausedAt).Seconds()
				b.pausedAt = nil
			}
		}
		open[sessionID] = remaining
	}
	all := func(*broadcastRow) bool { return true }

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		text := scanner.Text()
		if !strings.HasPrefix(text, "{") {
			continue
		}
		var line logLine
		if err := json.Unmarshal([]byte(text), &line); err != nil || line.SessionID == "" {
			continue
		}
		if line.Msg == "created live session" {
			if !until.IsZero() && !line.Time.Before(until) {
				continue
			}
			sessions[line.SessionID] = &sessionRow{ID: line.SessionID, UserID: line.UserID, StartedAt: line.Time}
			continue
		}
		s := sessions[line.SessionID]
		if s == nil {
			continue
		}
		switch line.Msg {
		case "closed live session":
			end := line.Time
			s.EndedAt, s.EndReason = &end, line.Reason
			// 세션이 끝나면 송출도 끝났다. 종료 로그가 없던 경로라 추정으로 표시한다.
			endOpen(s.ID, all, line.Time, "session_closed", true)
		case "RTMP egress started":
			provider := line.Provider
			if provider == "" {
				provider = providerFromURL(line.URL)
			}
			b := &broadcastRow{
				ID:        uuid.NewSHA1(broadcastNamespace, []byte(s.ID+"|"+provider+"|"+line.Time.Format(time.RFC3339Nano))).String(),
				SessionID: s.ID, Provider: provider, StartedAt: line.Time,
			}
			broadcasts = append(broadcasts, b)
			open[s.ID] = append(open[s.ID], b)
		case "RTMP egress stopped":
			match := all
			if line.Provider != "" {
				match = func(b *broadcastRow) bool { return b.Provider == line.Provider }
			}
			endOpen(s.ID, match, line.Time, line.Reason, false)
		case "RTMP egress stopped after terminal recovery failure":
			endOpen(s.ID, all, line.Time, line.StopReason, false)
		case "platform broadcast is live":
			// 치지직은 라이브 전환 뒤에 송출이 붙어 열린 송출이 없다 — started_at이 곧 라이브다.
			for _, b := range open[s.ID] {
				if b.LiveAt == nil {
					at := line.Time
					b.LiveAt = &at
				}
			}
		case "RTMP egress paused":
			for _, b := range open[s.ID] {
				if b.pausedAt == nil {
					at := line.Time
					b.pausedAt = &at
				}
			}
		case "RTMP egress resumed":
			for _, b := range open[s.ID] {
				if b.pausedAt != nil {
					b.PausedSeconds += line.Time.Sub(*b.pausedAt).Seconds()
					b.pausedAt = nil
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read log: %w", err)
	}

	ordered := make([]*sessionRow, 0, len(sessions))
	for _, s := range sessions {
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].StartedAt.Before(ordered[j].StartedAt) })
	return ordered, broadcasts, nil
}

func writeSQL(w io.Writer, sessions []*sessionRow, broadcasts []*broadcastRow) {
	fmt.Fprintln(w, "BEGIN;")
	for _, s := range sessions {
		// 탈퇴한 계정은 users에 없으므로 NULL로 남는다(익명화와 같은 결과).
		userExpr, isGuest := "NULL", "true"
		if s.UserID != "" && s.UserID != uuid.Nil.String() {
			userExpr, isGuest = fmt.Sprintf("(SELECT id FROM users WHERE id = %s)", quote(s.UserID)), "false"
		}
		endReason := s.EndReason
		if s.EndedAt == nil {
			endReason = "unclean_shutdown"
		}
		fmt.Fprintf(w, "INSERT INTO usage_sessions (session_id, user_id, is_guest, started_at, ended_at, end_reason, source) "+
			"VALUES (%s, %s, %s, %s, %s, %s, 'backfill') ON CONFLICT DO NOTHING;\n",
			quote(s.ID), userExpr, isGuest, quoteTime(&s.StartedAt), quoteTime(s.EndedAt), quote(endReason))
	}
	for _, b := range broadcasts {
		endReason := b.EndReason
		if b.EndedAt == nil {
			endReason = "unclean_shutdown"
		}
		// 서버가 이미 기록한 세션(source=live)에는 붙이지 않는다.
		fmt.Fprintf(w, "INSERT INTO usage_broadcasts (id, session_id, provider, started_at, live_at, ended_at, end_reason, "+
			"paused_seconds, source, ended_at_estimated) SELECT %s, %s, %s, %s, %s, %s, %s, %.3f, 'backfill', %t "+
			"WHERE EXISTS (SELECT 1 FROM usage_sessions WHERE session_id = %s AND source = 'backfill') ON CONFLICT DO NOTHING;\n",
			quote(b.ID), quote(b.SessionID), quote(b.Provider), quoteTime(&b.StartedAt), quoteTime(b.LiveAt),
			quoteTime(b.EndedAt), quote(endReason), b.PausedSeconds, b.Estimated, quote(b.SessionID))
	}
	fmt.Fprintln(w, "COMMIT;")
}

// providerFromURL은 provider 필드가 로그에 붙기 전(#232 이전)의 송출을 출력
// 호스트로 가른다. 치지직 인제스트는 네이버 호스트다.
func providerFromURL(outputURL string) string {
	if strings.Contains(outputURL, "navercorp.com") {
		return "chzzk"
	}
	return "youtube"
}

func quote(value string) string {
	if value == "" {
		return "NULL"
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func quoteTime(value *time.Time) string {
	if value == nil {
		return "NULL"
	}
	return "'" + value.UTC().Format(time.RFC3339Nano) + "'"
}
