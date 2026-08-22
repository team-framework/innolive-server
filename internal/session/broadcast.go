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
	case BroadcastPhasePreparing, BroadcastPhasePrepared:
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
	// BroadcastPhasePreparing: 플랫폼에 방송을 만드는 중이다. 이 구간에도
	// 설정 변경을 막아야 한다 — 준비가 읽어간 설정과 저장값이 갈리면 조회
	// 결과와 실제 방송이 어긋난다.
	BroadcastPhasePreparing BroadcastPhase = "preparing"
	// BroadcastPhasePrepared: 방송이 만들어졌지만 시청자에게 노출되지 않는다.
	BroadcastPhasePrepared BroadcastPhase = "prepared"
	// BroadcastPhaseGoingLive: 라이브 전환 요청을 플랫폼에 보낸 뒤 응답을
	// 기다리는 중이다. 이 구간에 들어온 중지는 전환 결과를 확인한 뒤에야
	// 마무리할 수 있다 — 이미 나간 요청은 취소되지 않기 때문이다.
	BroadcastPhaseGoingLive BroadcastPhase = "going_live"
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

// BeginBroadcastPrepare는 플랫폼 호출을 시작하기 전에 준비 구간을 선점한다.
// 플랫폼 왕복은 수 초가 걸리고, 그동안 설정이 바뀌면 만들어진 방송과 저장값이
// 어긋나므로 phase를 먼저 preparing으로 옮겨 설정 변경과 중복 준비를 막는다.
func (m *Manager) BeginBroadcastPrepare(id string) (*Session, error) {
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
	case BroadcastPhasePreparing, BroadcastPhasePrepared:
		return nil, ErrBroadcastPrepared
	case BroadcastPhaseLive:
		return nil, ErrBroadcastLive
	}
	s.broadcastPhase = BroadcastPhasePreparing
	s.UpdatedAt = time.Now().UTC()
	return s, nil
}

// ResetBroadcastPreparation은 선점한 준비 구간을 되돌린다. 플랫폼 준비가
// 실패했거나 egress를 붙이지 못했을 때 호출한다.
func (m *Manager) ResetBroadcastPreparation(id string) {
	s, err := m.Get(id)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broadcastPhase != BroadcastPhasePreparing {
		return
	}
	s.platformBroadcast = nil
	s.broadcastPhase = BroadcastPhaseIdle
	s.UpdatedAt = time.Now().UTC()
}

// MarkBroadcastPrepared는 플랫폼 방송 준비 결과를 세션에 기록한다.
// BeginBroadcastPrepare로 선점한 구간에서만 기록할 수 있다 — 방송 2개가 한
// 세션에 붙으면 어느 쪽이 라이브가 되는지 알 수 없다.
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
	case BroadcastPhasePreparing:
	default:
		return nil, ErrBroadcastNotPrepared
	}
	s.platformBroadcast = &broadcast
	s.broadcastPhase = BroadcastPhasePrepared
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("platform broadcast prepared", "session_id", s.ID,
		"provider", broadcast.Provider, "broadcast_id", broadcast.BroadcastID)
	return s, nil
}

// BeginGoLive는 라이브 전환 구간을 선점한다. 플랫폼에 요청을 보내고 나면
// 취소할 수 없으므로, 그 사이에 들어온 중지는 즉시 방송을 지우는 대신 중지
// 요청만 기록하고 전환 결과를 이 구간의 주인이 마무리한다.
func (m *Manager) BeginGoLive(id string) (*Session, error) {
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
	case BroadcastPhaseGoingLive:
		return nil, ErrBroadcastGoingLive
	case BroadcastPhasePrepared:
	default:
		return nil, ErrBroadcastNotPrepared
	}
	s.broadcastPhase = BroadcastPhaseGoingLive
	s.goLiveStopRequested = false
	s.UpdatedAt = time.Now().UTC()
	return s, nil
}

// CompleteGoLive는 플랫폼 전환이 성공한 뒤의 마무리다. 전환 도중 중지가
// 요청됐으면 aborted=true와 함께 방송 정보를 돌려준다 — 호출자는 그 방송을
// 즉시 종료시켜야 한다. 세션이 이미 사라진 경우도 중지로 본다.
func (m *Manager) CompleteGoLive(id string) (aborted bool, broadcast PlatformBroadcast, err error) {
	s, err := m.Get(id)
	if err != nil {
		return true, PlatformBroadcast{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.platformBroadcast != nil {
		broadcast = *s.platformBroadcast
	}
	if s.closed || s.goLiveStopRequested {
		s.platformBroadcast = nil
		s.broadcastPhase = BroadcastPhaseIdle
		s.goLiveStopRequested = false
		s.UpdatedAt = time.Now().UTC()
		m.logger.Info("go live aborted by stop", "session_id", s.ID, "broadcast_id", broadcast.BroadcastID)
		return true, broadcast, nil
	}
	if s.broadcastPhase != BroadcastPhaseGoingLive {
		return true, broadcast, ErrBroadcastNotPrepared
	}
	s.broadcastPhase = BroadcastPhaseLive
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("platform broadcast is live", "session_id", s.ID)
	return false, broadcast, nil
}

// AbortGoLive는 전환 요청이 실패했을 때 선점을 되돌린다. 그 사이 중지가
// 요청됐으면 준비 상태로 돌아가지 않고 방송을 넘겨준다 — 호출자가 지운다.
func (m *Manager) AbortGoLive(id string) (stopped bool, broadcast PlatformBroadcast) {
	s, err := m.Get(id)
	if err != nil {
		return true, PlatformBroadcast{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.platformBroadcast != nil {
		broadcast = *s.platformBroadcast
	}
	if s.broadcastPhase != BroadcastPhaseGoingLive {
		return true, broadcast
	}
	if s.closed || s.goLiveStopRequested {
		s.platformBroadcast = nil
		s.broadcastPhase = BroadcastPhaseIdle
		s.goLiveStopRequested = false
		s.UpdatedAt = time.Now().UTC()
		return true, broadcast
	}
	s.broadcastPhase = BroadcastPhasePrepared
	s.UpdatedAt = time.Now().UTC()
	return false, broadcast
}

// PlatformBroadcast는 준비된 방송과 그 단계의 스냅샷이다. preparing 구간에는
// 아직 방송 정보가 없으므로 방송은 zero value이고 단계만 유효하다.
func (s *Session) PlatformBroadcast() (PlatformBroadcast, BroadcastPhase) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.platformBroadcast == nil {
		return PlatformBroadcast{}, s.broadcastPhase
	}
	return *s.platformBroadcast, s.broadcastPhase
}
