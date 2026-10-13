package media

import (
	"testing"

	"inno-live-server/internal/config"
)

func TestOrientationChanged(t *testing.T) {
	tests := []struct {
		name                   string
		fromW, fromH, toW, toH uint16
		want                   bool
	}{
		{"portrait ramp-up", 360, 640, 720, 1280, false},
		{"landscape ramp-up", 640, 360, 1280, 720, false},
		{"portrait to landscape", 360, 640, 960, 540, true},
		{"landscape to portrait", 1280, 720, 180, 320, true},
		{"square is landscape", 640, 640, 1280, 720, false},
		{"square to portrait", 640, 640, 360, 640, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := orientationChanged(test.fromW, test.fromH, test.toW, test.toH); got != test.want {
				t.Fatalf("orientationChanged() = %v, want %v", got, test.want)
			}
		})
	}
}

// 방향 전환으로 디코더 출력 치수가 바뀌면 검은 프레임도 새 치수로 만들어야 한다(#449).
// 옛 치수의 검은 프레임을 내보내면 raw 인코더가 크기 불일치로 실패한다.
func TestBlackoutFrameFollowsFrameDimensions(t *testing.T) {
	processor := &Processor{wireFormat: config.WireFormatRaw}
	portrait, err := processor.blackoutFrame(nil, 720, 1280)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(portrait), rawFrameSize(720, 1280); got != want {
		t.Fatalf("portrait blackout = %d bytes, want %d", got, want)
	}
	landscape, err := processor.blackoutFrame(nil, 1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(landscape), rawFrameSize(1280, 720); got != want {
		t.Fatalf("landscape blackout = %d bytes, want %d", got, want)
	}
	square, err := processor.blackoutFrame(nil, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(square), rawFrameSize(64, 64); got != want {
		t.Fatalf("resized blackout = %d bytes, want %d", got, want)
	}
}
