package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func attachedSink(t *testing.T, pipe *AudioPipe, name string, muted bool) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	if err := pipe.Attach(file, muted); err != nil {
		t.Fatalf("attach %s: %v", name, err)
	}
	return file
}

// 대상마다 자기 Ogg 헤더와 자기 스트림을 받아야 한다. 파이프 하나를 둘이 읽으면
// Opus 프레임이 쪼개져 양쪽 다 깨지므로, 이 테스트는 각 출력을 실제로 디코드한다.
func TestAudioPipeFanoutWritesDecodableStreamPerTarget(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	pipe := NewAudioPipe(testLogger(), metrics.New(), 2)
	youtube := attachedSink(t, pipe, "youtube.ogg", false)
	chzzk := attachedSink(t, pipe, "chzzk.ogg", false)

	// 20ms 프레임 간격(48kHz에서 960 samples)으로 단조 증가시킨다.
	const samplesPerFrame = opusClockRate * opusSilenceFrameDuration / time.Second
	var timestamp uint32 = 160000
	for range 75 {
		pipe.writeSample(timestamp, opusSilenceFrame)
		timestamp += uint32(samplesPerFrame)
	}
	pipe.Detach(youtube)
	pipe.Detach(chzzk)

	for _, file := range []*os.File{youtube, chzzk} {
		output, err := exec.Command("ffmpeg", "-v", "error", "-i", file.Name(), "-f", "null", "-").CombinedOutput()
		if err != nil {
			t.Fatalf("%s is not decodable: %v\n%s", filepath.Base(file.Name()), err, output)
		}
		info, err := os.Stat(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", filepath.Base(file.Name()))
		}
	}
}

// 한쪽만 pause해도 나머지는 마이크를 계속 받는다. mute가 파이프 전역이면 한쪽을
// 멈추는 순간 양쪽이 같이 무음이 된다(#233에서 실제로 생기는 상황).
func TestAudioPipeFanoutMutesTargetsIndependently(t *testing.T) {
	registry := metrics.New()
	pipe := NewAudioPipe(testLogger(), registry, 2)
	live := attachedSink(t, pipe, "live.ogg", false)
	paused := attachedSink(t, pipe, "paused.ogg", true)

	pipe.writeSample(160000, opusSilenceFrame)
	if got := counterValue(t, registry, audioWrittenMetric); got != 1 {
		t.Fatalf("mic sample written = %v, want 1 (live target only)", got)
	}
	if got := counterValue(t, registry, audioDroppedMetric); got != 1 {
		t.Fatalf("mic sample dropped = %v, want 1 (paused target only)", got)
	}

	pipe.writeSilenceSamples()
	if got := counterValue(t, registry, audioWrittenMetric); got != 2 {
		t.Fatalf("after silence tick written = %v, want 2 (paused target only)", got)
	}
	if !pipe.Muted(paused) || pipe.Muted(live) {
		t.Fatalf("mute state leaked across targets: live=%v paused=%v", pipe.Muted(live), pipe.Muted(paused))
	}
}

// 한 대상의 스트림이 깨져도 나머지는 계속 나가야 한다.
func TestAudioPipeFanoutBrokenTargetDoesNotStopOthers(t *testing.T) {
	registry := metrics.New()
	pipe := NewAudioPipe(testLogger(), registry, 2)
	healthy := attachedSink(t, pipe, "healthy.ogg", false)
	broken := attachedSink(t, pipe, "broken.ogg", false)
	// write end를 미리 닫아 쓰기가 실패하게 만든다.
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}

	pipe.writeSample(160000, opusSilenceFrame)
	if pipe.Muted(broken) || pipe.sinkCount() != 1 {
		t.Fatalf("broken target must be detached; sinks=%d", pipe.sinkCount())
	}
	pipe.writeSample(161000, opusSilenceFrame)
	if got := counterValue(t, registry, audioWrittenMetric); got != 2 {
		t.Fatalf("healthy target writes = %v, want 2", got)
	}
	pipe.Detach(healthy)
}

// pause 중에 FFmpeg가 재연결하면 새 스트림도 무음으로 시작해야 한다. mute가
// 스트림마다 독립이라 이전 상태를 물려받지 않으므로, egress가 Attach에 의도를
// 다시 실어 준다 — 그러지 않으면 일시 중지한 방송에서 소리가 새어 나간다.
func TestEgressAttachMutesAudioWhileUserPaused(t *testing.T) {
	egress := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey")
	egress.setStreaming(1280, 720, 30)
	if !egress.Pause() {
		t.Fatal("Pause returned false")
	}
	if !egress.audioShouldMute() {
		t.Fatal("a reattach while paused must start muted")
	}
	if !egress.Resume() {
		t.Fatal("Resume returned false")
	}
	if egress.audioShouldMute() {
		t.Fatal("a reattach after resume must start unmuted")
	}
}
