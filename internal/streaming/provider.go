// Package streaming은 사용자별 송출 대상 플랫폼의 방송 라이프사이클을 다룬다.
// 계정 연결·토큰(internal/auth) 위에서 "방송 준비(Prepare)"와 "중지(Stop)"를
// 플랫폼 공통 계약으로 추상화한다 — 키 조회 수준으로 추상화하면 방송 생성
// API가 있는 YouTube와 없는 치지직이 한 구조에 들어가지 않기 때문이다.
package streaming

import (
	"context"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
)

// PrepareOptions는 방송 1회의 표시 속성이다.
type PrepareOptions struct {
	// Title은 플랫폼에 표시될 방송 제목. 비면 프로바이더 기본값.
	Title string
	// Privacy는 공개 범위(public/unlisted/private). 비면 프로바이더 기본값.
	Privacy string
	// MadeForKids는 아동용 콘텐츠 여부 신고값이다. 사용자가 직접 고른 값만
	// 플랫폼에 전달해야 하므로, "미선택"(nil)과 "아동용 아님"(false)을
	// 구분하려고 포인터를 쓴다. nil이면 프로바이더가 준비를 거절한다.
	MadeForKids *bool
	// Description은 방송 설명. 비면 설명 없이 만든다.
	Description string
	// CategoryID는 플랫폼 카테고리 id. 비면 카테고리를 설정하지 않는다.
	CategoryID string
	// Thumbnail은 업로드할 썸네일 원본. nil이면 올리지 않는다.
	Thumbnail *Thumbnail
	// AutoStart는 송출이 감지되면 플랫폼이 알아서 라이브로 넘기게 한다.
	// 준비와 라이브 전환을 분리한 뒤로 기본값은 false이고, 제거 예정인
	// POST /stream/start(#142)만 종전 동작을 위해 true로 켠다.
	AutoStart bool
}

// Thumbnail은 업로드할 썸네일 이미지다.
type Thumbnail struct {
	MIME string
	Data []byte
}

// Warning은 방송은 진행되었지만 선택 항목이 반영되지 않았음을 알린다.
// 카테고리·썸네일처럼 실패해도 송출을 막지 않는 항목에만 쓴다.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PreparedBroadcast는 송출 시작에 필요한 준비 결과다.
type PreparedBroadcast struct {
	Provider auth.StreamingProvider
	// IngestURL은 스트림 키가 포함된 완성 URL이다. 로그에 남기지 말 것.
	IngestURL   string
	BroadcastID string
	StreamID    string
	// Warnings는 선택 항목(카테고리·썸네일) 반영 실패 목록이다.
	Warnings []Warning
}

// BroadcastDefaults는 설정 폼의 초기값이다. 플랫폼에 "업로드 기본값"을 읽는
// API가 없어(#143) 직전 방송의 값을 그대로 쓴다. 썸네일은 URL 참조로 옮길 수
// 없어(다운로드 후 재업로드가 필요) 불러오지 않는다.
type BroadcastDefaults struct {
	Title       string
	Description string
	Privacy     string
	// MadeForKids는 PrepareOptions와 같은 이유로 포인터다 — 직전 방송에서
	// 읽지 못했으면 사용자가 직접 골라야 하는 미선택이다.
	MadeForKids *bool
	CategoryID  string
}

// FallbackDefaults는 직전 방송이 없거나 조회에 실패했을 때의 초기값이다.
// 설정 폼 자체는 언제나 열려야 하므로 호출자는 조회 실패를 이 값으로 덮는다.
func FallbackDefaults() BroadcastDefaults {
	return BroadcastDefaults{Title: defaultBroadcastTitle, Privacy: defaultBroadcastPrivacy}
}

// Provider는 플랫폼별 방송 라이프사이클 계약이다.
type Provider interface {
	// Prepare는 플랫폼 쪽 방송 준비를 마치고 송출 대상 ingest URL을 돌려준다.
	// YouTube: 재사용 스트림 확보 + broadcast 생성(autoStart/autoStop) + bind.
	// 치지직(미래): 스트림 키 조회만으로 완결(방송 생성 API 없음).
	Prepare(ctx context.Context, userID uuid.UUID, options PrepareOptions) (PreparedBroadcast, error)
	// GoLive는 준비된 방송을 시청자에게 공개되는 라이브 상태로 전환한다.
	// YouTube: liveBroadcasts.transition(live). 방송 생성 API가 없어 송출
	// 시작이 곧 라이브인 플랫폼(치지직 등)의 구현은 no-op이다.
	GoLive(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast) error
	// EndLive는 이미 라이브가 된 방송을 즉시 끝낸다. 전환 요청이 플랫폼에
	// 나간 뒤 중지가 들어온 경우, autoStop(실측 57.6초)을 기다리면 그동안
	// 시청자에게 노출되므로 직접 종료시킨다.
	EndLive(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast) error
	// Stop은 명시적 중지 시 플랫폼 쪽 마무리를 한다. 송출 중단 자체는
	// egress 종료가 담당하므로, 플랫폼 API 호출이 불필요한 구현은 no-op이다.
	// 라이브 전환 이후의 종료는 autoStop이 맡으므로, 호출자는 아직 라이브가
	// 되지 않은(prepare만 된) 방송에 대해서만 이걸 부른다.
	Stop(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast) error
	// Defaults는 설정 폼의 초기값을 돌려준다. 직전 방송이 없으면
	// FallbackDefaults()를 그대로 돌려준다(에러가 아니다).
	Defaults(ctx context.Context, userID uuid.UUID) (BroadcastDefaults, error)
}
