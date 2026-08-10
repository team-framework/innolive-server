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
}

// PreparedBroadcast는 송출 시작에 필요한 준비 결과다.
type PreparedBroadcast struct {
	Provider auth.StreamingProvider
	// IngestURL은 스트림 키가 포함된 완성 URL이다. 로그에 남기지 말 것.
	IngestURL   string
	BroadcastID string
	StreamID    string
}

// Provider는 플랫폼별 방송 라이프사이클 계약이다.
type Provider interface {
	// Prepare는 플랫폼 쪽 방송 준비를 마치고 송출 대상 ingest URL을 돌려준다.
	// YouTube: 재사용 스트림 확보 + broadcast 생성(autoStart/autoStop) + bind.
	// 치지직(미래): 스트림 키 조회만으로 완결(방송 생성 API 없음).
	Prepare(ctx context.Context, userID uuid.UUID, options PrepareOptions) (PreparedBroadcast, error)
	// Stop은 명시적 중지 시 플랫폼 쪽 마무리를 한다. 송출 중단 자체는
	// egress 종료가 담당하므로, 플랫폼 API 호출이 불필요한 구현은 no-op이다.
	Stop(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast) error
}
