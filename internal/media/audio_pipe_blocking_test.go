package media

import (
	"io"
	"os"
	"testing"
	"time"

	"inno-live-server/internal/metrics"
)

// 한 플랫폼 인제스트가 멈춰 FFmpeg가 pipe:3을 읽지 않으면 커널 파이프 버퍼가 차고
// Write가 막힌다. 막힌 대상만 떼어내야 다른 대상의 오디오와 mute 전환이 이어진다.
func TestAudioPipeDetachesBlockedSinkWithoutStallingOthers(t *testing.T) {
	registry := metrics.New()
	pipe := NewAudioPipe(testLogger(), registry, 2)

	stuckRead, stuckWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	liveRead, liveWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stuckRead.Close()
		_ = liveRead.Close()
	})
	go func() { _, _ = io.Copy(io.Discard, liveRead) }()

	if err := pipe.Attach(stuckWrite, false); err != nil {
		t.Fatal(err)
	}
	if err := pipe.Attach(liveWrite, false); err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, 4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		const samplesPerFrame = opusClockRate * opusSilenceFrameDuration / time.Second
		var timestamp uint32 = 160000
		// 파이프 버퍼(macOS·Linux 64 KiB)를 넘길 만큼 쓴다.
		for range 64 {
			pipe.writeSample(timestamp, payload)
			timestamp += uint32(samplesPerFrame)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = stuckRead.Close() // 막힌 Write를 풀어 테스트 고루틴을 정리한다.
		t.Fatal("writer stayed blocked on a sink whose reader stopped")
	}

	muted := make(chan struct{})
	go func() {
		pipe.SetMuted(liveWrite, true)
		close(muted)
	}()
	select {
	case <-muted:
	case <-time.After(time.Second):
		t.Fatal("SetMuted blocked behind the stuck sink")
	}

	if pipe.Muted(stuckWrite) || pipe.sinkCount() != 1 {
		t.Fatalf("sinks = %d, want only the live sink left", pipe.sinkCount())
	}
	if got := droppedValue(t, registry, audioDropPipeError); got < 1 {
		t.Fatalf("pipe_error drops = %v, want the stuck sink counted", got)
	}
}
