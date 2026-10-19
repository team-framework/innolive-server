package media

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"inno-live-server/internal/metrics"
)

// TestFFmpegProcessCloseDrainsUnreadOutput는 #497을 고정한다. 파이프라인 취소로
// 읽기 쪽이 출력을 남긴 채 떠나도 close가 남은 출력을 비워, 프로세스가 grace를
// 기다리지 않고 스스로 끝난다. 가짜 디코더는 FFmpeg처럼 SIGINT로 멈추지 않고,
// stdin이 닫히면 파이프 버퍼보다 큰 출력(2MB)을 쓴 뒤 정상 종료한다.
func TestFFmpegProcessCloseDrainsUnreadOutput(t *testing.T) {
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), metrics.New(), TranscoderOptions{})
	transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"trap '' INT; echo ready >&2; cat > /dev/null; dd if=/dev/zero bs=65536 count=32 2>/dev/null")
	}
	ready := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	process, err := transcoder.startFFmpeg(ctx, "decoder", func(line string) {
		if line == "ready" {
			ready <- struct{}{}
		}
	}, nil)
	if err != nil {
		t.Fatalf("startFFmpeg() error = %v", err)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("fake decoder did not install its SIGINT trap")
	}

	// 파이프라인 취소와 같은 순서다. 컨텍스트 취소가 SIGINT를 보내고, 읽기 쪽은
	// stdout을 읽지 않은 채 close를 부른다.
	cancel()
	started := time.Now()
	process.close()
	elapsed := time.Since(started)

	if elapsed >= ffmpegShutdownGrace/2 {
		t.Fatalf("close() took %v, want well under the %v shutdown grace", elapsed, ffmpegShutdownGrace)
	}
	if state := process.cmd.ProcessState; state == nil || !state.Success() {
		t.Fatalf("process state = %v, want a clean exit instead of a kill", state)
	}
}
