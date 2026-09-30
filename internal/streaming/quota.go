package streaming

import (
	"net/http"
	"sync"
	"time"
	// 컨테이너 이미지에 tzdata가 없어도 태평양 시간(쿼터 초기화 기준)을 계산한다.
	_ "time/tzdata"
)

const (
	// YouTubeDailyQuota는 프로젝트 기본 일일 쿼터다. 모든 사용자가 나눠 쓴다.
	YouTubeDailyQuota = 10000
	// youtubeQuotaLowRatio를 넘으면 백그라운드 주기 조회를 쉬고 남은 쿼터를 방송
	// 시작·종료·전환에 남긴다(#361).
	youtubeQuotaLowRatio = 0.8
)

var quotaResetZone = func() *time.Location {
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.UTC
	}
	return zone
}()

// QuotaMeter는 이 프로세스가 오늘(태평양 시간) 쓴 YouTube API 쿼터 추정치다.
// 공식 단가(조회 1, 생성·연결·전환·삭제·수정·업로드 50)로 호출마다 더한다.
// 재시작하면 0부터 다시 세므로 콘솔 수치보다 작을 수 있다 — 하한 추정이다.
type QuotaMeter struct {
	mu    sync.Mutex
	day   string
	units int
	now   func() time.Time
}

// NewQuotaMeter는 빈 추정치를 만든다.
func NewQuotaMeter() *QuotaMeter {
	return &QuotaMeter{now: time.Now}
}

// quotaCost는 호출 하나의 단가다. YouTube Data API에서 읽기(GET)는 1, 쓰기는
// 전부 50이다(liveBroadcasts·liveStreams·videos·thumbnails).
func quotaCost(method string) int {
	if method == http.MethodGet {
		return 1
	}
	return 50
}

// Record는 호출 하나를 센다.
func (m *QuotaMeter) Record(method string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollLocked()
	m.units += quotaCost(method)
}

func (m *QuotaMeter) rollLocked() {
	day := m.now().In(quotaResetZone).Format("2006-01-02")
	if day != m.day {
		m.day, m.units = day, 0
	}
}

// Used는 오늘 쓴 쿼터 추정치다.
func (m *QuotaMeter) Used() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollLocked()
	return m.units
}

// Low는 부가 조회를 꺼야 할 만큼 썼는지다.
func (m *QuotaMeter) Low() bool {
	return float64(m.Used()) >= youtubeQuotaLowRatio*YouTubeDailyQuota
}

// QuotaReporter는 플랫폼 API 쿼터를 추정하는 프로바이더다. 선택 구현이다.
type QuotaReporter interface {
	Quota() *QuotaMeter
}

// Quota는 이 프로바이더의 쿼터 추정치다.
func (p *YouTubeProvider) Quota() *QuotaMeter { return p.quota }
