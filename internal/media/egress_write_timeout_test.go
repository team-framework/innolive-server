package media

import (
	"context"
	"os"
	"testing"
	"time"

	"inno-live-server/internal/config"
)

// RTMP 인제스트가 멈추면 FFmpeg가 stdin을 읽지 않는다. 쓰기가 기한 없이 막히면
// egress는 streaming으로 보이는 채 재연결도 하지 못한다(P1-5).
func TestEgressWriteFramesFailsWhenFFmpegStopsReading(t *testing.T) {
	e := newTestEgress(config.WireFormatRaw, "out.flv")
	e.setStreaming(320, 180, 30)
	e.writeTimeout = 100 * time.Millisecond
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})
	data := make([]byte, rawFrameSize(320, 180))
	pending := make([]frame, 4)
	for i := range pending {
		pending[i] = frame{data: data, width: 320, height: 180, privacyGeneration: 1}
	}

	result := make(chan error, 1)
	go func() {
		_, _, err := e.writeFrames(context.Background(), &ffmpegProcess{stdin: writeEnd}, pending, 320, 180, 30, func() {})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("writeFrames returned without an error while FFmpeg was not reading")
		}
	case <-time.After(8 * time.Second):
		_ = readEnd.Close() // 막힌 Write를 풀어 고루틴을 정리한다.
		t.Fatal("writeFrames stayed blocked on a stalled FFmpeg stdin")
	}
}
