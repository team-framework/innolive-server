package media

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
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

// TestFFmpegProcessCloseReturnsWhileGrandchildHoldsStdout는 close의 stdout 비우기가
// 프로세스 회수 뒤에 끝나는지 본다. 손자 프로세스가 stdout 쓰기 끝을 쥐고 있으면
// 자식이 끝나도 EOF가 오지 않으므로, Wait가 읽기 끝을 닫아 비우기를 풀어야 한다.
// 그렇지 않으면 close가 손자가 끝날 때까지 멈춘다.
func TestFFmpegProcessCloseReturnsWhileGrandchildHoldsStdout(t *testing.T) {
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), metrics.New(), TranscoderOptions{})
	transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $! >&2; exit 0")
	}
	grandchild := make(chan int, 1)
	process, err := transcoder.startFFmpeg(context.Background(), "decoder", func(line string) {
		if pid, err := strconv.Atoi(line); err == nil {
			grandchild <- pid
		}
	}, nil)
	if err != nil {
		t.Fatalf("startFFmpeg() error = %v", err)
	}
	select {
	case pid := <-grandchild:
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	case <-time.After(5 * time.Second):
		t.Fatal("fake decoder did not start its grandchild")
	}

	returned := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		process.close()
		returned <- time.Since(started)
	}()
	select {
	case elapsed := <-returned:
		if elapsed >= ffmpegShutdownGrace/2 {
			t.Fatalf("close() took %v, want well under the %v shutdown grace", elapsed, ffmpegShutdownGrace)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close() did not return while a grandchild held stdout open")
	}
	if state := process.cmd.ProcessState; state == nil || !state.Success() {
		t.Fatalf("process state = %v, want the child's clean exit", state)
	}
}

// TestFFmpegProcessCloseLogsSIGKILL는 grace를 넘겨 kill된 FFmpeg가 역할과 함께
// 경고 로그를 남기고, 스스로 끝난 FFmpeg는 남기지 않는지 본다.
func TestFFmpegProcessCloseLogsSIGKILL(t *testing.T) {
	for _, test := range []struct {
		name   string
		script string
		want   int
	}{
		{name: "killed", script: "kill -9 $$", want: 1},
		{name: "clean exit", script: "cat > /dev/null", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs capturedLog
			transcoder := NewFFmpegTranscoder("ffmpeg", logs.logger(), metrics.New(), TranscoderOptions{})
			transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", test.script)
			}
			process, err := transcoder.startFFmpeg(context.Background(), "decoder", nil, nil)
			if err != nil {
				t.Fatalf("startFFmpeg() error = %v", err)
			}
			process.close()

			records := logs.records(t, "FFmpeg did not exit within the shutdown grace and was killed")
			if len(records) != test.want {
				t.Fatalf("kill logs = %v, want %d", records, test.want)
			}
			if test.want > 0 && records[0]["ffmpeg_role"] != "decoder" {
				t.Fatalf("kill log role = %v, want decoder", records[0]["ffmpeg_role"])
			}
		})
	}
}
