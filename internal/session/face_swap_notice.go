package session

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// FaceSwapDescriptionNotice는 얼굴 합성 세션의 방송 설명 맨 앞에 서버가 붙이는
// 안내문이다. 시청자가 합성 사실을 알 수 있도록 사용자 입력과 무관하게 싣는다.
const FaceSwapDescriptionNotice = "※ 이 방송에는 AI를 이용한 얼굴 합성이 포함되어 있습니다."

// faceSwapNoticeSeparator는 안내문과 사용자 설명 사이의 빈 줄이다.
const faceSwapNoticeSeparator = "\n\n"

// PlatformDescription은 플랫폼에 보낼 방송 설명이다. 저장값은 사용자 입력 그대로
// 두고 보낼 때만 안내문을 붙인다 — 조회 응답과 재저장에 안내문이 섞이지 않게 한다.
func PlatformDescription(faceAnonymization, description string) string {
	if faceAnonymization != FaceAnonymizationFaceSwap {
		return description
	}
	if description == "" {
		return FaceSwapDescriptionNotice
	}
	return FaceSwapDescriptionNotice + faceSwapNoticeSeparator + description
}

// StripFaceSwapNotice는 직전 방송 설명에서 서버가 붙인 안내문을 뗀다. 직전 값이
// 폼 초기값이 되므로, 떼지 않으면 블러 방송에 안내문이 남거나 합성 방송에 두 번 붙는다.
func StripFaceSwapNotice(description string) string {
	rest, found := strings.CutPrefix(description, FaceSwapDescriptionNotice)
	if !found {
		return description
	}
	return strings.TrimPrefix(rest, faceSwapNoticeSeparator)
}

// validateDescriptionFor는 안내문을 붙인 뒤에도 유튜브 설명 한도 안인지 본다.
// 넘치면 방송 준비에서 플랫폼이 거절하므로 저장 시점에 막는다.
func validateDescriptionFor(faceAnonymization, description string) error {
	if faceAnonymization != FaceAnonymizationFaceSwap {
		return nil
	}
	limit := MaxYouTubeDescriptionLength - utf8.RuneCountInString(FaceSwapDescriptionNotice+faceSwapNoticeSeparator)
	if utf8.RuneCountInString(description) > limit {
		return InvalidBroadcastSettingsError{Field: "description", Reason: fmt.Sprintf("must be at most %d characters for face swap sessions", limit)}
	}
	return nil
}

// ValidateFor는 Validate에 세션의 얼굴 가림 방식에 따른 설명 한도를 더한다.
func (b YouTubeBroadcastSettings) ValidateFor(faceAnonymization string) error {
	if err := b.Validate(); err != nil {
		return err
	}
	return validateDescriptionFor(faceAnonymization, b.Description)
}
