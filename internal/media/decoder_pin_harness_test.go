//go:build egress_harness

// 디코더 핀(#122)의 실 ffmpeg 검증. 단위 테스트는 인자 조립까지만 보므로,
// 여기서는 실제로 디코드된 페이로드의 치수가 핀에 고정되는지를 본다.
//
//	go test -tags egress_harness -run TestDecoderPin ./internal/media -v
package media

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg"
	"log/slog"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// drainDecoderFrames는 drainDecoder와 같지만 프레임을 버리지 않고 모은다.
// 핀 검증은 메타데이터와 페이로드를 함께 봐야 하기 때문이다.
func drainDecoderFrames(t *testing.T, transcoder *FFmpegTranscoder, units [][]byte) (string, []frame) {
	t.Helper()
	logs := &syncLogBuffer{}
	transcoder.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	input := make(chan frame, 512)
	output := make(chan frame, 512)
	timestamp := uint32(90000)
	for _, data := range units {
		input <- frame{data: data, timestamp: timestamp}
		timestamp += 3000
	}
	close(input)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() { _ = transcoder.DecodeStream(ctx, input, output); close(output) }()

	var collected []frame
	for item := range output {
		collected = append(collected, item)
	}
	return logs.String(), collected
}

// assertPinned는 모든 프레임의 메타데이터와 실제 JPEG 페이로드가 핀 치수와
// 일치하는지 본다. 이 두 값이 어긋나면 하위 단계가 조용히 깨진다 — 핀이
// 지켜야 하는 불변식이 바로 "메타데이터 == 페이로드"다.
func assertPinned(t *testing.T, frames []frame, wantWidth, wantHeight uint16) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("디코드된 프레임이 없다")
	}
	for i, item := range frames {
		if item.width != wantWidth || item.height != wantHeight {
			t.Fatalf("프레임 %d 메타데이터 = %dx%d, want %dx%d", i, item.width, item.height, wantWidth, wantHeight)
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(item.data))
		if err != nil {
			t.Fatalf("프레임 %d 디코드 실패: %v", i, err)
		}
		if config.Width != int(wantWidth) || config.Height != int(wantHeight) {
			t.Fatalf("프레임 %d 페이로드 = %dx%d, want %dx%d (메타데이터와 어긋남)",
				i, config.Width, config.Height, wantWidth, wantHeight)
		}
	}
	t.Logf("%d프레임 전부 메타데이터 == 페이로드 == %dx%d", len(frames), wantWidth, wantHeight)
}

// TestDecoderPinHoldsH264RiseLandscape는 가로 소스가 램프업으로 올라가도
// 출력이 핀에 고정되는지 본다. 핀이 없으면 첫 프레임 규격에 묶인다.
func TestDecoderPinHoldsH264RiseLandscape(t *testing.T) {
	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	stream := append(
		annexBStream(t, dir, "small.h264", 320, 180),
		annexBStream(t, dir, "big.h264", 1280, 720)...,
	)
	units := splitAccessUnits(t, stream)
	if len(units) == 0 {
		t.Fatal("액세스 유닛 0개")
	}

	transcoder := NewFFmpegTranscoder("ffmpeg", nil, metrics.New(), TranscoderOptions{
		VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG, PinLongEdge: 1280,
	})
	output, frames := drainDecoderFrames(t, transcoder, units)
	assertPinned(t, frames, 1280, 720)
	assertLogged(t, output, "H.264 decoder output locked", "width=1280", "height=720", "pin_long_edge=1280")
}

// TestDecoderPinHoldsVP8RisePortrait는 프로덕션 실사용 경로(VP8)와 다수
// 방향(세로)을 함께 본다. 세로 소스는 핀이 720x1280으로 유도돼야 한다.
func TestDecoderPinHoldsVP8RisePortrait(t *testing.T) {
	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	frames := append(
		vp8Frames(t, dir, "small.ivf", 180, 320),
		vp8Frames(t, dir, "big.ivf", 720, 1280)...,
	)
	if len(frames) == 0 {
		t.Fatal("VP8 프레임 0개")
	}

	transcoder := NewFFmpegTranscoder("ffmpeg", nil, metrics.New(), TranscoderOptions{
		VideoCodec: VideoCodecVP8, WireFormat: config.WireFormatJPEG, PinLongEdge: 1280,
	})
	output, decoded := drainDecoderFrames(t, transcoder, frames)
	assertPinned(t, decoded, 720, 1280)
	assertLogged(t, output, "VP8 decoder output locked", "width=720", "height=1280", "pin_long_edge=1280")
}

// TestDecoderPinSurvivesMidStreamRotation은 방송 도중 기기를 돌린 경우를 본다.
// 핀 치수는 첫 프레임 방향으로 정해지고, 이후 방향이 뒤집혀도 pad가 흡수해
// 출력 규격이 유지돼야 한다 — 유지되지 않으면 하위 단계가 전부 깨진다.
func TestDecoderPinSurvivesMidStreamRotation(t *testing.T) {
	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	stream := append(
		annexBStream(t, dir, "landscape.h264", 640, 360),
		annexBStream(t, dir, "portrait.h264", 360, 640)...,
	)
	units := splitAccessUnits(t, stream)

	transcoder := NewFFmpegTranscoder("ffmpeg", nil, metrics.New(), TranscoderOptions{
		VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG, PinLongEdge: 1280,
	})
	_, frames := drainDecoderFrames(t, transcoder, units)
	assertPinned(t, frames, 1280, 720)
}

// TestDecoderPinOffKeepsFirstFrameDimensions는 기본값(OFF)에서 종전 동작이
// 그대로인지 확인한다. 이 테스트가 깨지면 머지만으로 프로덕션이 바뀐다.
func TestDecoderPinOffKeepsFirstFrameDimensions(t *testing.T) {
	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	stream := append(
		annexBStream(t, dir, "small.h264", 320, 180),
		annexBStream(t, dir, "big.h264", 1280, 720)...,
	)
	units := splitAccessUnits(t, stream)

	transcoder := NewFFmpegTranscoder("ffmpeg", nil, metrics.New(), TranscoderOptions{
		VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG,
	})
	output, frames := drainDecoderFrames(t, transcoder, units)
	assertPinned(t, frames, 320, 180)
	assertLogged(t, output, "H.264 decoder output locked", "width=320", "height=180", "pin_long_edge=0")
}
