package media

import "testing"

// TestMeasureFPSSurvivesSlowStart는 세션 초반에 프레임률 자체가 낮았다가
// 안정화되는 램프업을 다룬다. #127이 다룬 산발적인 드롭과는 상황이 다르다 —
// 그때는 간격이 가끔 벌어지지만, 여기서는 초반 구간 전체가 균일하게 느리다.
//
// 측정 창이 egressMeasureFrames장이라 간격은 그보다 하나 적고, 중앙값은 그
// 절반 지점을 고른다. 즉 느린 구간이 간격의 절반만 만들어도 중앙값이 통째로
// 그쪽으로 넘어간다. 느린 구간은 시간당 프레임을 적게 만들므로, 역설적으로
// 아주 느릴 때보다 어중간하게 느릴 때(10~20fps) 더 쉽게 넘어간다.
func TestMeasureFPSSurvivesSlowStart(t *testing.T) {
	// slowStart는 앞의 slowFrames장을 slowFPS 간격으로, 나머지를 fastFPS
	// 간격으로 배치한다. lateDrops는 빠른 구간에서 추가로 누락시킬 장수다 —
	// 램프업에서는 저프레임 구간과 산발 드롭이 함께 나타난다.
	//
	// 90kHz 정수 틱으로 계산한다. 초 단위 부동소수점을 누적하면 경계 케이스에서
	// 느린 프레임이 한 장 밀려 표의 라벨과 실제 타임라인이 어긋난다.
	slowStart := func(slowFPS, slowFrames, fastFPS, lateDrops int) []frame {
		slowStep := uint32(videoClockRate / slowFPS)
		fastStep := uint32(videoClockRate / fastFPS)
		out := make([]frame, 0, egressMeasureFrames)
		ts := uint32(videoClockRate)
		dropped := 0
		for i := 0; len(out) < egressMeasureFrames; i++ {
			step := fastStep
			if i < slowFrames-1 {
				step = slowStep
			}
			if i >= slowFrames && dropped < lateDrops && (i-slowFrames)%3 == 1 {
				dropped++
				ts += step
				continue
			}
			out = append(out, frame{timestamp: ts})
			ts += step
		}
		return out
	}

	tests := []struct {
		name  string
		input []frame
		want  int
	}{
		{"초반 7장이 7fps", slowStart(7, 7, 30, 0), 30},
		{"초반 10장이 10fps", slowStart(10, 10, 30, 0), 30},
		{"초반 15장이 15fps", slowStart(15, 15, 30, 0), 30},
		{"초반 20장이 20fps", slowStart(20, 20, 30, 0), 30},
		{"초반 15장이 15fps + 이후 드롭 1회", slowStart(15, 15, 30, 1), 30},
		{"초반 15장이 15fps + 이후 드롭 2회", slowStart(15, 15, 30, 2), 30},
		{"초반 21장이 7fps", slowStart(7, 21, 30, 0), 30},
		{"초반 16장이 7fps (중앙값이 넘어가는 경계)", slowStart(7, 16, 30, 0), 30},
		// 60fps 소스가 느리게 시작해도 60을 지켜야 한다. 프레임률을 기본값으로
		// 뭉개는 방식이었다면 여기서 30이 나온다.
		{"60fps 소스가 초반 15장만 30fps", slowStart(30, 15, 60, 0), 60},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := measureFPS(tc.input); got != tc.want {
				t.Fatalf("measureFPS = %d, want %d", got, tc.want)
			}
		})
	}
}
