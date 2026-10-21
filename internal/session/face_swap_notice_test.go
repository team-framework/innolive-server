package session

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// 얼굴 합성 세션만 설명 앞에 안내문이 붙고, 직전 설명에서 떼면 사용자 입력으로 돌아온다.
func TestPlatformDescriptionAddsNoticeOnlyForFaceSwap(t *testing.T) {
	for _, tc := range []struct {
		mode, description, want string
	}{
		{FaceAnonymizationBlur, "설명", "설명"},
		{"", "설명", "설명"},
		{FaceAnonymizationFaceSwap, "", FaceSwapDescriptionNotice},
		{FaceAnonymizationFaceSwap, "설명", FaceSwapDescriptionNotice + "\n\n설명"},
	} {
		got := PlatformDescription(tc.mode, tc.description)
		if got != tc.want {
			t.Fatalf("PlatformDescription(%q, %q) = %q, want %q", tc.mode, tc.description, got, tc.want)
		}
		if stripped := StripFaceSwapNotice(got); stripped != tc.description {
			t.Fatalf("StripFaceSwapNotice(%q) = %q, want %q", got, stripped, tc.description)
		}
	}
	if got := StripFaceSwapNotice("사용자 설명 " + FaceSwapDescriptionNotice); got != "사용자 설명 "+FaceSwapDescriptionNotice {
		t.Fatalf("notice in the middle must stay, got %q", got)
	}
}

// 안내문을 붙인 결과가 유튜브 한도를 넘는 설명은 합성 세션에서만 저장 시점에 막는다.
func TestValidateForReservesNoticeLength(t *testing.T) {
	limit := MaxYouTubeDescriptionLength - utf8.RuneCountInString(FaceSwapDescriptionNotice+"\n\n")
	fits := YouTubeBroadcastSettings{Description: strings.Repeat("가", limit)}
	over := YouTubeBroadcastSettings{Description: strings.Repeat("가", limit+1)}
	if err := fits.ValidateFor(FaceAnonymizationFaceSwap); err != nil {
		t.Fatalf("limit-length description rejected: %v", err)
	}
	if utf8.RuneCountInString(PlatformDescription(FaceAnonymizationFaceSwap, fits.Description)) != MaxYouTubeDescriptionLength {
		t.Fatal("limit must leave exactly the notice length")
	}
	var invalid InvalidBroadcastSettingsError
	if err := over.ValidateFor(FaceAnonymizationFaceSwap); !errors.As(err, &invalid) || invalid.Field != "description" {
		t.Fatalf("err = %v, want description invalid", err)
	}
	if err := over.ValidateFor(FaceAnonymizationBlur); err != nil {
		t.Fatalf("blur session must keep the plain limit: %v", err)
	}
}
