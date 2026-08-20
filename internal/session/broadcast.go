package session

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 방송 설정 한계값. 제목·설명은 YouTube liveBroadcasts.insert의 문자 수 제한,
// 썸네일 크기는 thumbnails.set의 업로드 제한이다.
const (
	MaxYouTubeTitleLength       = 100
	MaxYouTubeDescriptionLength = 5000
	MaxYouTubeThumbnailBytes    = 2 << 20
)

// broadcastPrivacyValues는 status.privacyStatus 열거값이다.
var broadcastPrivacyValues = map[string]bool{"public": true, "unlisted": true, "private": true}

// thumbnailMIMETypes는 thumbnails.set이 받는 이미지 형식이다.
var thumbnailMIMETypes = map[string]bool{"image/jpeg": true, "image/png": true}

// YouTubeThumbnail은 업로드된 썸네일 원본이다. thumbnails.set이 URL 참조를 지원하지
// 않아 바이트를 그대로 들고 있다가 방송 준비 때 멀티파트로 올린다.
type YouTubeThumbnail struct {
	MIME string
	Data []byte
}

// YouTubeBroadcastSettings는 세션 1회 방송의 사용자 입력 설정이다. 저장
// 시점에는 플랫폼을 호출하지 않고, 방송 준비(Prepare)에서 사용된다.
type YouTubeBroadcastSettings struct {
	Title       string
	Description string
	Privacy     string
	// MadeForKids는 법적 신고 항목이라 "미선택"(nil)과 "아동용 아님"(false)을
	// 구분한다. streaming.PrepareOptions와 같은 이유의 포인터다.
	MadeForKids *bool
	CategoryID  string
	Thumbnail   *YouTubeThumbnail
	UpdatedAt   time.Time
}

// InvalidBroadcastSettingsError는 검증에 실패한 필드를 지목한다. 호출자가
// 400 응답의 details에 필드명을 그대로 실을 수 있게 한다.
type InvalidBroadcastSettingsError struct {
	Field  string
	Reason string
}

func (e InvalidBroadcastSettingsError) Error() string {
	return fmt.Sprintf("invalid broadcast setting %s: %s", e.Field, e.Reason)
}

// Validate는 저장 전 검증이다. 빈 값은 "설정하지 않음"으로 허용하고,
// 값이 있을 때만 형식을 확인한다 — 필수 여부는 송출 시작 시점의 계약이다.
func (b YouTubeBroadcastSettings) Validate() error {
	if utf8.RuneCountInString(b.Title) > MaxYouTubeTitleLength {
		return InvalidBroadcastSettingsError{Field: "title", Reason: fmt.Sprintf("must be at most %d characters", MaxYouTubeTitleLength)}
	}
	if utf8.RuneCountInString(b.Description) > MaxYouTubeDescriptionLength {
		return InvalidBroadcastSettingsError{Field: "description", Reason: fmt.Sprintf("must be at most %d characters", MaxYouTubeDescriptionLength)}
	}
	if b.Privacy != "" && !broadcastPrivacyValues[b.Privacy] {
		return InvalidBroadcastSettingsError{Field: "privacy", Reason: "must be one of public, unlisted, private"}
	}
	if b.CategoryID != "" && !isDigits(b.CategoryID) {
		// 실제 카테고리 존재 여부는 videos.update가 판정한다(선택 항목이라
		// 실패해도 방송은 진행된다). 여기서는 형식만 막는다.
		return InvalidBroadcastSettingsError{Field: "category_id", Reason: "must be a numeric YouTube category id"}
	}
	if b.Thumbnail != nil {
		if !thumbnailMIMETypes[b.Thumbnail.MIME] {
			return InvalidBroadcastSettingsError{Field: "thumbnail.mime", Reason: "must be image/jpeg or image/png"}
		}
		if len(b.Thumbnail.Data) == 0 {
			return InvalidBroadcastSettingsError{Field: "thumbnail.data", Reason: "must not be empty"}
		}
		if len(b.Thumbnail.Data) > MaxYouTubeThumbnailBytes {
			return InvalidBroadcastSettingsError{Field: "thumbnail.data", Reason: fmt.Sprintf("must be at most %d bytes", MaxYouTubeThumbnailBytes)}
		}
	}
	return nil
}

func isDigits(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// YouTubeBroadcastResponse는 조회 응답의 방송 설정 표현이다. 썸네일 원본은 수 MB라
// 되돌려주지 않고 메타데이터만 노출한다.
type YouTubeBroadcastResponse struct {
	Title       string                    `json:"title"`
	Description string                    `json:"description"`
	Privacy     string                    `json:"privacy"`
	MadeForKids *bool                     `json:"made_for_kids"`
	CategoryID  string                    `json:"category_id"`
	Thumbnail   *YouTubeThumbnailResponse `json:"thumbnail"`
	UpdatedAt   time.Time                 `json:"updated_at"`
}

type YouTubeThumbnailResponse struct {
	MIME  string `json:"mime"`
	Bytes int    `json:"bytes"`
}

func (b YouTubeBroadcastSettings) response() YouTubeBroadcastResponse {
	response := YouTubeBroadcastResponse{
		Title:       b.Title,
		Description: b.Description,
		Privacy:     b.Privacy,
		MadeForKids: b.MadeForKids,
		CategoryID:  b.CategoryID,
		UpdatedAt:   b.UpdatedAt,
	}
	if b.Thumbnail != nil {
		response.Thumbnail = &YouTubeThumbnailResponse{MIME: b.Thumbnail.MIME, Bytes: len(b.Thumbnail.Data)}
	}
	return response
}

// SetBroadcastSettings는 검증된 방송 설정을 세션에 저장한다. 플랫폼 호출은
// 하지 않는다. 방송이 이미 준비된 뒤에는 거절한다 — 저장값을 바꿔도 만들어진
// 방송에는 반영되지 않아 설정과 실물이 어긋나기 때문이다(#142).
func (m *Manager) SetBroadcastSettings(id string, settings YouTubeBroadcastSettings) (*Session, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrNotFound
	}
	switch s.broadcastPhase {
	case BroadcastPhasePrepared:
		return nil, ErrBroadcastPrepared
	case BroadcastPhaseLive:
		return nil, ErrBroadcastLive
	}
	settings.UpdatedAt = time.Now().UTC()
	s.broadcast = &settings
	s.UpdatedAt = settings.UpdatedAt
	m.logger.Info("broadcast settings updated", "session_id", s.ID,
		"privacy", settings.Privacy, "has_thumbnail", settings.Thumbnail != nil)
	return s, nil
}

// BroadcastSettings는 저장된 설정의 스냅샷이다. 저장된 값이 없으면 zero value.
func (s *Session) BroadcastSettings() YouTubeBroadcastSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.broadcast == nil {
		return YouTubeBroadcastSettings{}
	}
	return *s.broadcast
}

// BroadcastPhase는 플랫폼 방송이 라이프사이클의 어디에 있는지다. egress는
// "YouTube에 방송이 준비만 되어 있는" 상태를 알 수 없어(egress phase에서 파생
// 불가) 세션이 직접 들고 있는다.
type BroadcastPhase string

const (
	// BroadcastPhaseIdle: 플랫폼 방송이 없다.
	BroadcastPhaseIdle BroadcastPhase = "idle"
	// BroadcastPhasePrepared: 방송이 만들어졌지만 시청자에게 노출되지 않는다.
	BroadcastPhasePrepared BroadcastPhase = "prepared"
	// BroadcastPhaseLive: 라이브로 전환되어 시청자에게 노출된다.
	BroadcastPhaseLive BroadcastPhase = "live"
)

// PlatformBroadcast는 준비된 플랫폼 방송의 식별 정보다. 라이브 전환·정리에
// 필요한 값만 담는다 — ingest URL은 스트림 키를 포함하므로 넣지 않는다.
type PlatformBroadcast struct {
	Provider    string
	BroadcastID string
	StreamID    string
}

// MarkBroadcastPrepared는 플랫폼 방송 준비 결과를 세션에 기록한다. 이미
// 준비되었거나 라이브인 세션은 거절한다 — 방송 2개가 한 세션에 붙으면 어느
// 쪽이 라이브가 되는지 알 수 없다.
func (m *Manager) MarkBroadcastPrepared(id string, broadcast PlatformBroadcast) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrNotFound
	}
	switch s.broadcastPhase {
	case BroadcastPhasePrepared:
		return nil, ErrBroadcastPrepared
	case BroadcastPhaseLive:
		return nil, ErrBroadcastLive
	}
	s.platformBroadcast = &broadcast
	s.broadcastPhase = BroadcastPhasePrepared
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("platform broadcast prepared", "session_id", s.ID,
		"provider", broadcast.Provider, "broadcast_id", broadcast.BroadcastID)
	return s, nil
}

// MarkBroadcastLive는 준비된 방송이 라이브로 전환되었음을 기록한다.
func (m *Manager) MarkBroadcastLive(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrNotFound
	}
	switch s.broadcastPhase {
	case BroadcastPhaseLive:
		return nil, ErrBroadcastLive
	case BroadcastPhasePrepared:
	default:
		return nil, ErrBroadcastNotPrepared
	}
	s.broadcastPhase = BroadcastPhaseLive
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("platform broadcast is live", "session_id", s.ID)
	return s, nil
}

// PlatformBroadcast는 준비된 방송과 그 단계의 스냅샷이다. 준비된 방송이 없으면
// phase는 idle이고 방송 정보는 zero value다.
func (s *Session) PlatformBroadcast() (PlatformBroadcast, BroadcastPhase) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.platformBroadcast == nil {
		return PlatformBroadcast{}, BroadcastPhaseIdle
	}
	return *s.platformBroadcast, s.broadcastPhase
}
