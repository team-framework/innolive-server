package media

import (
	"context"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"inno-live-server/internal/config"
)

// TestSetReconnectMaxElapsed: 치지직처럼 유예가 짧은 플랫폼을 위해 호출자가
// 예산을 줄일 수 있어야 한다. media는 어느 플랫폼인지 알지 않는다.
func TestSetReconnectMaxElapsed(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	if e.ReconnectMaxElapsed() != egressReconnectMaxElapsed {
		t.Fatalf("default = %v, want %v", e.ReconnectMaxElapsed(), egressReconnectMaxElapsed)
	}

	e.SetReconnectMaxElapsed(10 * time.Second)
	if e.ReconnectMaxElapsed() != 10*time.Second {
		t.Fatalf("after override = %v, want 10s", e.ReconnectMaxElapsed())
	}
	// 0은 "지정하지 않음"이다. 옵션을 넘기지 않은 호출자가 예산을 0으로
	// 만들어 첫 실패에 바로 포기하게 되면 안 된다.
	e.SetReconnectMaxElapsed(0)
	if e.ReconnectMaxElapsed() != 10*time.Second {
		t.Fatalf("zero must not clear the budget, got %v", e.ReconnectMaxElapsed())
	}
	e.SetReconnectMaxElapsed(-time.Second)
	if e.ReconnectMaxElapsed() != 10*time.Second {
		t.Fatalf("negative must not clear the budget, got %v", e.ReconnectMaxElapsed())
	}
}

// TestRunHonorsOverriddenReconnectBudget: 줄인 예산이 실제 Run 루프에서
// 지켜지고, 소진 시 rtmp_reconnect_exhausted로 끝나야 한다. 이 경로가 깨지면
// 치지직에서 유예가 지난 뒤 재연결에 성공해 방송이 하나 더 생긴다.
func TestRunHonorsOverriddenReconnectBudget(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://host/live2/secret-key")
	// 공개 API로만 예산을 줄인다 — 서버가 실제로 쓰는 경로다. backoff는
	// 테스트 속도를 위해 내부 값을 짧게 둔다.
	// backoff(20ms)보다 예산(30ms)이 짧아야 "시도 횟수"가 아니라 "경과 시간"이
	// 먼저 끊는 것을 확인할 수 있다.
	e.reconnectPolicy.minBackoff = 20 * time.Millisecond
	e.reconnectPolicy.maxBackoff = 20 * time.Millisecond
	e.SetReconnectMaxElapsed(30 * time.Millisecond)

	var starts atomic.Int32
	e.transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "__innolive_missing_ffmpeg_for_budget_test__")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	go func() {
		for index := 0; index < egressMeasureFrames; index++ {
			e.input <- frame{timestamp: uint32(index * 3000), width: 640, height: 360}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within the shortened budget")
	}

	status := e.Status()
	if status.StopReason == nil || *status.StopReason != EgressStopReasonReconnectExhausted {
		t.Fatalf("StopReason = %v, want %q", status.StopReason, EgressStopReasonReconnectExhausted)
	}
	// 경과 시간이 먼저 끊었다면 최대 시도 횟수(6)에는 닿지 못한다.
	if attempts := status.ReconnectAttempts; attempts >= egressReconnectMaxAttempts {
		t.Fatalf("ReconnectAttempts = %d, want the elapsed budget to cut it short", attempts)
	}
}
