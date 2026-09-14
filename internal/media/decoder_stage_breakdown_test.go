//go:build egress_harness

// 디코드 스테이지 분해(#177)의 실 ffmpeg 검증. "decode" 하나로 뭉쳐 있던
// 구간을 pre_decode_wait(큐 대기 + stdin 백프레셔)와 ffmpeg_decode(디코더
// 왕복)로 가르는 두 관측이 실제 디코드 경로에서 프레임마다 남는지를 본다.
//
//	go test -tags egress_harness -run TestDecoderStageBreakdown ./internal/media -v
package media

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"testing"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func TestDecoderStageBreakdownObservesBothStages(t *testing.T) {
	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	frames := vp8Frames(t, dir, "clip.ivf", 480, 640)
	if len(frames) == 0 {
		t.Fatal("VP8 프레임 0개")
	}

	registry := metrics.New()
	transcoder := NewFFmpegTranscoder("ffmpeg", nil, registry, TranscoderOptions{
		VideoCodec: VideoCodecVP8, WireFormat: config.WireFormatJPEG,
	})
	_, decoded := drainDecoderFrames(t, transcoder, frames)
	if len(decoded) == 0 {
		t.Fatal("디코드된 프레임이 없다")
	}

	var buf bytes.Buffer
	registry.WritePrometheus(&buf)
	metricsText := buf.String()

	// 두 스테이지 모두 디코드된 프레임 수만큼 관측돼야 한다. decode(기존)는
	// keepLatestDecoded 하류에서 관측되므로 여기 출력에는 없다 — 이 경로는
	// 디코더 단독이라 pre_decode_wait/ffmpeg_decode만 남는 게 정상이다.
	for _, stage := range []string{"pre_decode_wait", "ffmpeg_decode"} {
		count := stageCount(t, metricsText, stage)
		if count != len(decoded) {
			t.Fatalf("stage %q count = %d, want %d (디코드 프레임 수와 일치해야 함)",
				stage, count, len(decoded))
		}
	}
}

// stageCount는 Prometheus 텍스트에서 한 스테이지 히스토그램의 관측 횟수를 읽는다.
func stageCount(t *testing.T, metricsText, stage string) int {
	t.Helper()
	pattern := fmt.Sprintf(`innolive_ai_stage_duration_seconds_count\{stage=%q\} (\d+)`, stage)
	match := regexp.MustCompile(pattern).FindStringSubmatch(metricsText)
	if match == nil {
		t.Fatalf("stage %q 관측이 metrics 출력에 없다", stage)
	}
	count, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("stage %q count 파싱 실패: %v", stage, err)
	}
	return count
}
