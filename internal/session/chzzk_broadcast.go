package session

import (
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// 치지직 방송 설정 한계값. 2026-09-19 치지직 스튜디오 방송 설정 화면과 실제
// 입력으로 확인한 실측치다 — 공개 API 문서에는 숫자 상한이 없다.
//
// 제목은 한글·영문이 섞인 100자 문자열이 정확히 100에서 끊겨 바이트가 아니라
// 문자 수로 센다는 것까지 확인했다(아래 검증이 쓰는 기준과 같다).
//
// 상한을 아예 두지 않는 선택지도 있었으나, 플랫폼이 거절할 입력을 방송 직전
// (prepare)까지 들고 가면 "저장은 됐는데 방송이 안 되는" 상태가 된다.
const (
	MaxChzzkTitleLength = 100
	MaxChzzkTags        = 5
	MaxChzzkTagLength   = 15
)

// chzzkCategoryTypes는 lives/setting의 category.categoryType 열거값이다.
var chzzkCategoryTypes = map[string]bool{"GAME": true, "SPORTS": true, "ETC": true}

// ChzzkBroadcastSettings는 치지직 1회 방송의 사용자 입력 설정이다.
// 유튜브와 필드 교집합이 Title 하나뿐이라 공통 모델을 두지 않고 분리한다(D3).
// 설명·공개범위·썸네일·아동용 신고는 치지직에 없는 개념이다.
type ChzzkBroadcastSettings struct {
	Title string
	// CategoryType과 CategoryID는 함께 간다 — 치지직 category는 둘의 쌍이다.
	CategoryType string
	CategoryID   string
	Tags         []string
	UpdatedAt    time.Time
}

// Validate는 저장 전 검증이다. 유튜브 쪽과 같은 원칙으로, 빈 값은 "설정하지
// 않음"으로 허용하고 값이 있을 때만 형식을 본다.
func (b ChzzkBroadcastSettings) Validate() error {
	if utf8.RuneCountInString(b.Title) > MaxChzzkTitleLength {
		return InvalidBroadcastSettingsError{Field: "title", Reason: fmt.Sprintf("must be at most %d characters", MaxChzzkTitleLength)}
	}
	if b.CategoryType != "" && !chzzkCategoryTypes[b.CategoryType] {
		return InvalidBroadcastSettingsError{Field: "category_type", Reason: "must be one of GAME, SPORTS, ETC"}
	}
	// 카테고리는 타입과 id의 쌍이라 한쪽만 있으면 플랫폼이 받지 않는다.
	// 지목하는 필드는 "빠진 쪽"이어야 한다 — 클라이언트가 details.field로 폼
	// 항목을 하이라이트하므로, 멀쩡한 쪽을 지목하면 사용자가 원인을 못 찾는다.
	if b.CategoryType == "" && b.CategoryID != "" {
		return InvalidBroadcastSettingsError{Field: "category_type", Reason: "category_type and category_id must be set together"}
	}
	if b.CategoryType != "" && b.CategoryID == "" {
		return InvalidBroadcastSettingsError{Field: "category_id", Reason: "category_type and category_id must be set together"}
	}
	if len(b.Tags) > MaxChzzkTags {
		return InvalidBroadcastSettingsError{Field: "tags", Reason: fmt.Sprintf("must be at most %d tags", MaxChzzkTags)}
	}
	for index, tag := range b.Tags {
		field := fmt.Sprintf("tags[%d]", index)
		if tag == "" {
			return InvalidBroadcastSettingsError{Field: field, Reason: "must not be empty"}
		}
		if utf8.RuneCountInString(tag) > MaxChzzkTagLength {
			return InvalidBroadcastSettingsError{Field: field, Reason: fmt.Sprintf("must be at most %d characters", MaxChzzkTagLength)}
		}
		// 치지직 태그는 공백과 특수문자를 받지 않는다. 서버가 조용히 잘라내면
		// 사용자가 저장했다고 믿은 태그와 실제 방송이 달라지므로 거절한다.
		for _, r := range tag {
			if unicode.IsSpace(r) {
				return InvalidBroadcastSettingsError{Field: field, Reason: "must not contain whitespace"}
			}
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				return InvalidBroadcastSettingsError{Field: field, Reason: "must contain only letters and digits"}
			}
		}
	}
	return nil
}

// ChzzkBroadcastResponse는 조회 응답의 치지직 방송 설정 표현이다.
type ChzzkBroadcastResponse struct {
	Title        string    `json:"title"`
	CategoryType string    `json:"category_type"`
	CategoryID   string    `json:"category_id"`
	Tags         []string  `json:"tags"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (b ChzzkBroadcastSettings) response() ChzzkBroadcastResponse {
	// tags는 JSON에서 null이 아니라 빈 배열이어야 한다 — 클라이언트가
	// 분기 없이 순회할 수 있게(유튜브 목록 응답과 같은 판단).
	tags := append([]string{}, b.Tags...)
	return ChzzkBroadcastResponse{
		Title:        b.Title,
		CategoryType: b.CategoryType,
		CategoryID:   b.CategoryID,
		Tags:         tags,
		UpdatedAt:    b.UpdatedAt,
	}
}

// SetChzzkBroadcastSettings는 치지직 방송 설정을 세션에 저장한다. 단계 가드는
// 유튜브와 같다 — 준비가 읽어간 설정과 저장값이 갈리면 조회 결과와 실제
// 방송이 어긋난다(#142).
func (m *Manager) SetChzzkBroadcastSettings(id string, settings ChzzkBroadcastSettings) (*Session, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.primaryTarget()
	if s.closed {
		return nil, ErrNotFound
	}
	switch t.phase {
	case BroadcastPhasePreparing, BroadcastPhasePrepared:
		return nil, ErrBroadcastPrepared
	case BroadcastPhaseLive:
		return nil, ErrBroadcastLive
	}
	settings.UpdatedAt = time.Now().UTC()
	s.chzzkBroadcast = &settings
	s.UpdatedAt = settings.UpdatedAt
	s.lastActivityAt = settings.UpdatedAt
	// 제목·태그는 사용자 입력이라 로그에 값을 싣지 않는다.
	m.logger.Info("chzzk broadcast settings updated", "session_id", s.ID,
		"category_type", settings.CategoryType, "tag_count", len(settings.Tags))
	return s, nil
}

// ChzzkBroadcastSettings는 저장된 설정의 스냅샷이다. 저장된 값이 없으면 zero value.
func (s *Session) ChzzkBroadcastSettings() ChzzkBroadcastSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.chzzkBroadcast == nil {
		return ChzzkBroadcastSettings{}
	}
	// 구조체 복사만으로는 Tags 슬라이스가 세션 내부 배열을 계속 가리킨다.
	// 이 값은 prepare 옵션에 실려 프로바이더로 나가므로, 프로바이더가 태그를
	// 정규화(정렬·중복 제거)하면 저장된 사용자 설정이 조용히 바뀐다.
	snapshot := *s.chzzkBroadcast
	// nil은 nil로 남긴다 — "설정하지 않음"과 "빈 목록"의 구분을 스냅샷이
	// 임의로 바꾸지 않게.
	if s.chzzkBroadcast.Tags != nil {
		snapshot.Tags = append([]string{}, s.chzzkBroadcast.Tags...)
	}
	return snapshot
}
