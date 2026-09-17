package session

import (
	"errors"
	"strings"
	"testing"
)

func TestChzzkBroadcastSettingsValidate(t *testing.T) {
	cases := []struct {
		name      string
		settings  ChzzkBroadcastSettings
		wantField string
	}{
		{"empty is allowed", ChzzkBroadcastSettings{}, ""},
		{"full settings", ChzzkBroadcastSettings{
			Title: "테스트 방송", CategoryType: "GAME", CategoryID: "GTA5",
			Tags: []string{"게임", "고전명작", "retro2"},
		}, ""},
		{"title too long", ChzzkBroadcastSettings{Title: strings.Repeat("가", 101)}, "title"},
		{"unknown category type", ChzzkBroadcastSettings{CategoryType: "MUSIC", CategoryID: "x"}, "category_type"},
		{"category type without id", ChzzkBroadcastSettings{CategoryType: "GAME"}, "category_id"},
		{"category id without type", ChzzkBroadcastSettings{CategoryID: "GTA5"}, "category_type"},
		{"tag with space", ChzzkBroadcastSettings{Tags: []string{"고전 명작"}}, "tags[0]"},
		{"tag with special character", ChzzkBroadcastSettings{Tags: []string{"retro!"}}, "tags[0]"},
		{"tag with underscore", ChzzkBroadcastSettings{Tags: []string{"retro_game"}}, "tags[0]"},
		{"empty tag", ChzzkBroadcastSettings{Tags: []string{"게임", ""}}, "tags[1]"},
		{"tag too long", ChzzkBroadcastSettings{Tags: []string{strings.Repeat("가", 21)}}, "tags[0]"},
		{"too many tags", ChzzkBroadcastSettings{Tags: []string{
			"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10", "a11",
		}}, "tags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.settings.Validate()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var invalid InvalidBroadcastSettingsError
			if !errors.As(err, &invalid) {
				t.Fatalf("Validate() = %v, want InvalidBroadcastSettingsError", err)
			}
			if invalid.Field != tc.wantField {
				t.Fatalf("field = %q, want %q", invalid.Field, tc.wantField)
			}
		})
	}
}

// TestChzzkBroadcastResponseTagsAreArray: 태그가 없을 때 JSON이 null이 아니라
// 빈 배열이어야 클라이언트가 분기 없이 순회할 수 있다.
func TestChzzkBroadcastResponseTagsAreArray(t *testing.T) {
	response := ChzzkBroadcastSettings{Title: "t"}.response()
	if response.Tags == nil {
		t.Fatal("tags must serialize as [] rather than null")
	}
	if len(response.Tags) != 0 {
		t.Fatalf("tags = %v, want empty", response.Tags)
	}
}

// TestChzzkBroadcastSnapshotDoesNotAliasTags: 스냅샷의 Tags가 세션 내부
// 배열을 가리키면, prepare 옵션을 받은 프로바이더가 태그를 정규화하는 것만으로
// 저장된 사용자 설정이 조용히 바뀐다.
func TestChzzkBroadcastSnapshotDoesNotAliasTags(t *testing.T) {
	stored := ChzzkBroadcastSettings{Tags: []string{"나중", "먼저"}}
	s := &Session{chzzkBroadcast: &stored}

	snapshot := s.ChzzkBroadcastSettings()
	snapshot.Tags[0] = "변조됨"

	if got := s.ChzzkBroadcastSettings().Tags[0]; got != "나중" {
		t.Fatalf("stored tag = %q, want the session state to be untouched", got)
	}
	// 응답 경로도 같은 배열을 노출하면 안 된다.
	response := s.ChzzkBroadcastSettings().response()
	response.Tags[0] = "또 변조"
	if got := s.ChzzkBroadcastSettings().Tags[0]; got != "나중" {
		t.Fatalf("stored tag = %q after mutating the response copy", got)
	}
}

// TestChzzkCategoryPairErrorNamesMissingField: 쌍 검증은 빠진 쪽을 지목해야
// 한다 — 클라이언트가 details.field로 폼 항목을 하이라이트한다.
func TestChzzkCategoryPairErrorNamesMissingField(t *testing.T) {
	var invalid InvalidBroadcastSettingsError
	if err := (ChzzkBroadcastSettings{CategoryID: "GTA5"}).Validate(); !errors.As(err, &invalid) || invalid.Field != "category_type" {
		t.Fatalf("error = %v, want the missing category_type named", err)
	}
	if err := (ChzzkBroadcastSettings{CategoryType: "GAME"}).Validate(); !errors.As(err, &invalid) || invalid.Field != "category_id" {
		t.Fatalf("error = %v, want the missing category_id named", err)
	}
}
