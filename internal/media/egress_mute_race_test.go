package media

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func newMuteRaceEgress(t *testing.T) (*RTMPEgress, *os.File) {
	t.Helper()
	egress := newTestEgress(config.WireFormatJPEG, "rtmp://example.invalid/live/key")
	egress.audio = NewAudioPipe(testLogger(), metrics.New(), 2)
	egress.setStreaming(1280, 720, 30)
	file, err := os.Create(filepath.Join(t.TempDir(), "audio.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { egress.audio.Detach(file) })
	return egress, file
}

// FFmpeg 재연결로 새 스트림을 붙이는 동안 사용자가 일시 중지하면, 어느 쪽이
// 먼저 끝나든 새 스트림은 무음이어야 한다. 판정과 적용이 갈라져 있으면 Pause의
// 적용이 아직 없는 스트림에 떨어지고, Attach는 그 전에 내린 판정(마이크)으로
// 붙는다(P0-6).
func TestEgressAttachAndPauseRaceEndsMuted(t *testing.T) {
	for i := range 2000 {
		egress, file := newMuteRaceEgress(t)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := egress.attachAudio(file); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			egress.Pause()
		}()
		wg.Wait()
		if !egress.audio.Muted(file) {
			t.Fatalf("iteration %d: paused egress attached an unmuted stream", i)
		}
	}
}

// 재개와 입력 복구 시작이 겹쳐도 입력 복구 중이면 무음이어야 한다. Resume이
// 판정을 잠금 밖에서 하면 복구 시작의 무음을 늦게 덮어쓴다.
func TestEgressResumeAndInputRecoveryRaceEndsMuted(t *testing.T) {
	for i := range 2000 {
		egress, file := newMuteRaceEgress(t)
		if err := egress.attachAudio(file); err != nil {
			t.Fatal(err)
		}
		egress.Pause()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			egress.Resume()
		}()
		go func() {
			defer wg.Done()
			egress.BeginInputRecovery()
		}()
		wg.Wait()
		if !egress.audio.Muted(file) {
			t.Fatalf("iteration %d: input recovery left the microphone open", i)
		}
	}
}
