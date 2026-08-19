package media

import "testing"

func TestPinDimensions(t *testing.T) {
	tests := []struct {
		name                 string
		srcW, srcH, longEdge uint16
		wantW, wantH         uint16
	}{
		// 프로덕션 60일 26건에서 실제로 관측된 디코더 고정 해상도들.
		{"세로 270x480 (42%, 최빈)", 270, 480, 1280, 720, 1280},
		{"세로 360x640 (34%)", 360, 640, 1280, 720, 1280},
		{"가로 640x360 (15%)", 640, 360, 1280, 1280, 720},
		{"세로 180x320 (3%)", 180, 320, 1280, 720, 1280},
		{"가로 320x180 (3%)", 320, 180, 1280, 1280, 720},
		// 램프업 중간 단계 — 같은 비율이면 같은 핀이 나와야 한다.
		{"가로 960x540", 960, 540, 1280, 1280, 720},
		{"세로 540x960", 540, 960, 1280, 720, 1280},
		// 이미 핀보다 큰 소스는 축소된다.
		{"가로 1920x1080", 1920, 1080, 1280, 1280, 720},
		{"세로 1080x1920", 1080, 1920, 1280, 720, 1280},
		// 16:9가 아닌 비율도 여백 없이 담긴다.
		{"세로 480x640 (3:4)", 480, 640, 1280, 960, 1280},
		{"가로 640x480 (4:3)", 640, 480, 1280, 1280, 960},
		{"정사각 512x512", 512, 512, 1280, 1280, 1280},
		// 짝수 보정. 335*1280/1000 = 428.8 → 429(홀수) → 430으로 올린다.
		{"짝수 보정이 일어나는 비율", 1000, 335, 1280, 1280, 430},
		// 반올림 결과가 이미 짝수면 그대로 둔다(333 → 426.24 → 426).
		{"보정이 불필요한 비율", 1000, 333, 1280, 1280, 426},
		// 극단 비율에서도 짧은 변이 최소치 아래로 가지 않는다.
		{"초광각 3840x60", 3840, 60, 1280, 1280, 32},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, h, err := pinDimensions(tc.srcW, tc.srcH, tc.longEdge)
			if err != nil {
				t.Fatalf("pinDimensions: %v", err)
			}
			if w != tc.wantW || h != tc.wantH {
				t.Fatalf("pinDimensions(%d, %d, %d) = %dx%d, want %dx%d",
					tc.srcW, tc.srcH, tc.longEdge, w, h, tc.wantW, tc.wantH)
			}
			if w%2 != 0 || h%2 != 0 {
				t.Fatalf("치수가 홀수다: %dx%d", w, h)
			}
			if w != tc.longEdge && h != tc.longEdge {
				t.Fatalf("긴 변이 %d가 아니다: %dx%d", tc.longEdge, w, h)
			}
			// 소스와 핀의 방향이 같아야 한다.
			if (tc.srcW > tc.srcH) != (w > h) && tc.srcW != tc.srcH {
				t.Fatalf("방향이 뒤집혔다: 소스 %dx%d → 핀 %dx%d", tc.srcW, tc.srcH, w, h)
			}
		})
	}
}

func TestPinDimensionsRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		srcW, srcH, longEdge uint16
	}{
		{"소스 폭 0", 0, 480, 1280},
		{"소스 높이 0", 640, 0, 1280},
		{"장변 상한이 최소치 미만", 640, 360, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := pinDimensions(tc.srcW, tc.srcH, tc.longEdge); err == nil {
				t.Fatal("에러를 기대했다")
			}
		})
	}
}

func TestPinFilter(t *testing.T) {
	got := pinFilter(720, 1280)
	want := "scale=720:1280:force_original_aspect_ratio=decrease,pad=720:1280:-1:-1"
	if got != want {
		t.Fatalf("pinFilter = %q, want %q", got, want)
	}
}
