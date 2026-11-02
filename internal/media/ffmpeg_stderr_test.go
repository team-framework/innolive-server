package media

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/metrics"
)

// stderr 한 줄이 스캐너 한도를 넘으면 스캐너가 멈추고, 아무도 읽지 않는 stderr
// 파이프가 차서 FFmpeg가 쓰기에서 멈춘다. 그러면 종료가 grace를 다 쓰고 kill로
// 끝난다(P2-5). 남은 stderr를 버리면 프로세스가 스스로 끝나고, 마지막 줄도 남는다.
func TestFFmpegStderrIsDrainedPastOverlongLines(t *testing.T) {
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), metrics.New(), TranscoderOptions{})
	transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		// 100KB짜리 한 줄, 1MB의 추가 출력, 마지막 오류 줄을 stderr로 쓰고 끝난다.
		return exec.CommandContext(ctx, "sh", "-c",
			`head -c 102400 /dev/zero | tr '\0' x >&2; echo >&2; head -c 1048576 /dev/zero | tr '\0' y >&2; echo >&2; echo last-error >&2`)
	}
	var mu sync.Mutex
	var lines []string
	process, err := transcoder.startFFmpeg(context.Background(), "test", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	time.Sleep(200 * time.Millisecond)
	process.close()
	if elapsed := time.Since(started); elapsed >= ffmpegShutdownGrace {
		t.Fatalf("close took %s, want the process to exit on its own", elapsed)
	}
	if killedBySIGKILL(process.cmd.ProcessState) {
		t.Fatal("process was killed instead of exiting after its stderr was drained")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range lines {
		if strings.Contains(line, "x") || strings.Contains(line, "y") {
			t.Fatalf("overlong line was passed to the handler (len %d)", len(line))
		}
	}
}
