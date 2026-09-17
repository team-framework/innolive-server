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
		{"category id without type", ChzzkBroadcastSettings{CategoryID: "GTA5"}, "category_id"},
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
