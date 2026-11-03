package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) Printf(format string, values ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, values...))
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// GORM 기본 로거는 느린 쿼리와 디버그 로그에 값을 치환한 SQL을 남긴다. 토큰 해시·
// 이메일 같은 값이 로그로 나간다(P1-18).
func TestGormLogsParameterizedSQL(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL logger test")
	}
	captured := &capturedLog{}
	previous := gormLogWriter
	gormLogWriter = captured
	t.Cleanup(func() { gormLogWriter = previous })

	connection, err := Open(context.Background(), Options{
		URL: databaseURL, MaxOpenConns: 1, MaxIdleConns: 1,
		ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, Debug: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	var count int64
	if err := connection.DB.Table("pg_catalog.pg_namespace").Where("nspname = ?", "secret-refresh-hash").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	output := captured.String()
	if !strings.Contains(output, "SELECT") {
		t.Fatalf("debug log did not record the query: %q", output)
	}
	if strings.Contains(output, "secret-refresh-hash") {
		t.Fatalf("SQL log exposed a bound value: %q", output)
	}
}
