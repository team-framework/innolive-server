package media

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// newGapTestEgress는 공백 채우기 시간을 짧게 줄인 egress와, 그 규격의 슬레이트와
// 다른 JPEG 카메라 프레임을 돌려준다.
func newGapTestEgress(t *testing.T) (*RTMPEgress, []byte, []byte) {
	t.Helper()
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	e.gapFillAfter = 30 * time.Millisecond
	e.freezeMax = 150 * time.Millisecond
	e.setStreaming(320, 180, 60)
	slate, err := e.cancellationSlate(320, 180)
	if err != nil {
		t.Fatalf("cancellationSlate: %v", err)
	}
	camera := append([]byte(nil), slate.data...)
	camera[len(camera)/2] ^= 0x01
	if !isJPEG(camera) {
		t.Fatal("test camera frame must remain JPEG-shaped")
	}
	return e, camera, slate.data
}

// kinds는 쓰인 프레임을 순서대로 c(카메라)·s(슬레이트)로 적는다.
func (w *recordingWriteCloser) kinds(camera, slate []byte) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []byte
	for _, written := range w.writes {
		switch {
		case bytes.Equal(written, camera):
			out = append(out, 'c')
		case bytes.Equal(written, slate):
			out = append(out, 's')
		default:
			out = append(out, '?')
		}
	}
	return string(out)
}

func runGapWriteFrames(t *testing.T, e *RTMPEgress, pending []frame, duration time.Duration) *recordingWriteCloser {
	t.Helper()
	writer := &recordingWriteCloser{}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	_, _, err := e.writeFrames(ctx, &ffmpegProcess{stdin: writer}, pending, 320, 180, 60, func() {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writeFrames error = %v, want context deadline", err)
	}
	return writer
}

func TestInputGapHoldsLastFrameThenSwitchesToSlate(t *testing.T) {
	e, camera, slate := newGapTestEgress(t)
	writer := runGapWriteFrames(t, e, []frame{{data: camera, width: 320, height: 180, privacyGeneration: 1}}, 400*time.Millisecond)

	kinds := writer.kinds(camera, slate)
	first := bytes.IndexByte([]byte(kinds), 's')
	if first < 0 || bytes.Count([]byte(kinds[:first]), []byte("c")) < 2 {
		t.Fatalf("writes = %q, want the last frame held before the slate", kinds)
	}
	if bytes.ContainsAny([]byte(kinds[first:]), "c?") {
		t.Fatalf("writes = %q, want only slate after the freeze limit", kinds)
	}
}

func TestInputGapUsesSlateWhenFrameCannotBeHeld(t *testing.T) {
	for _, test := range []struct {
		name       string
		generation uint64
		invalidate uint64
	}{
		// 처리 도중 익명화 설정이 바뀐 프레임.
		{name: "changed while processing", generation: 0},
		// 익명화 설정이 바뀌기 전에 처리된 프레임.
		{name: "processed before setting change", generation: 1, invalidate: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, camera, slate := newGapTestEgress(t)
			e.InvalidateHeldFrame(test.invalidate)
			writer := runGapWriteFrames(t, e, []frame{{data: camera, width: 320, height: 180, privacyGeneration: test.generation}}, 200*time.Millisecond)

			kinds := writer.kinds(camera, slate)
			if len(kinds) < 2 || kinds[0] != 'c' || bytes.ContainsAny([]byte(kinds[1:]), "c?") {
				t.Fatalf("writes = %q, want one camera frame then slate only", kinds)
			}
		})
	}
}

func TestInputGapInvalidatedWhileHoldingSwitchesToSlate(t *testing.T) {
	e, camera, slate := newGapTestEgress(t)
	e.freezeMax = time.Second
	writer := &recordingWriteCloser{}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = e.writeFrames(ctx, &ffmpegProcess{stdin: writer}, []frame{{data: camera, width: 320, height: 180, privacyGeneration: 1}}, 320, 180, 60, func() {})
	}()
	waitFor(t, func() bool { return writer.count() >= 3 })
	e.InvalidateHeldFrame(2)
	<-done

	kinds := writer.kinds(camera, slate)
	first := bytes.IndexByte([]byte(kinds), 's')
	if first < 0 || bytes.ContainsAny([]byte(kinds[first:]), "c?") {
		t.Fatalf("writes = %q, want slate only after invalidation", kinds)
	}
}

func TestInputGapMutesAudioUntilCameraReturns(t *testing.T) {
	e, camera, _ := newGapTestEgress(t)
	e.freezeMax = time.Second
	writer := &recordingWriteCloser{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = e.writeFrames(ctx, &ffmpegProcess{stdin: writer}, []frame{{data: camera, width: 320, height: 180, privacyGeneration: 1}}, 320, 180, 60, func() {})
	}()
	waitFor(t, e.audioShouldMute)
	e.Enqueue(frame{data: camera, width: 320, height: 180, privacyGeneration: 1})
	waitFor(t, func() bool { return !e.audioShouldMute() })
	cancel()
	<-done
}

func TestProcessorPrivacyGenerationChangesOnlyWithSetting(t *testing.T) {
	processor, err := NewProcessor(config.PrivacyModeReal, 0, &fakeAIStream{}, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	initial := processor.PrivacyGeneration()
	if initial == 0 {
		t.Fatal("initial generation = 0, want a holdable generation")
	}
	processor.SetAnonymizationEnabled(true)
	if got := processor.PrivacyGeneration(); got != initial {
		t.Fatalf("generation after unchanged setting = %d, want %d", got, initial)
	}
	processor.SetAnonymizationEnabled(false)
	if got := processor.PrivacyGeneration(); got <= initial {
		t.Fatalf("generation after setting change = %d, want > %d", got, initial)
	}

	// 새 파이프라인의 Processor는 이전 세대보다 크다 — 무효화 기준을 넘는다.
	next, err := NewProcessor(config.PrivacyModeReal, 0, &fakeAIStream{}, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if next.PrivacyGeneration() <= processor.PrivacyGeneration() {
		t.Fatalf("new processor generation = %d, want > %d", next.PrivacyGeneration(), processor.PrivacyGeneration())
	}
}

// 처리 루프는 프레임에 처리한 세대를 찍는다. 처리 중 설정이 바뀐 프레임은
// 새 세대로 무효화한 egress에서 정지 화면 대상이 아니어야 한다.
func TestProcessImagesStampsPrivacyGeneration(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blocking := false
	processor, err := NewProcessor(config.PrivacyModeReal, 0, &fakeAIStream{process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		if blocking {
			entered <- struct{}{}
			<-release
		}
		return &aiv1.ProcessedVideoChunk{Data: []byte("output"), Timestamp: timestamp, StatusMessage: "success"}, nil
	}}, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	registry := metrics.New()
	sink := newFanoutSink(registry)
	fanout := NewEgressFanout()
	fanout.Add("chzzk", sink)
	run := func() frame {
		decoded := make(chan frame, 1)
		decoded <- frame{data: []byte("camera-frame"), width: 640, height: 480}
		close(decoded)
		processImages(context.Background(), nil, processor, decoded, make(chan frame, 1), fanout, registry, config.PrivacyModeReal, nil)
		return <-sink.input
	}

	if got := run().privacyGeneration; got != processor.PrivacyGeneration() {
		t.Fatalf("steady frame generation = %d, want %d", got, processor.PrivacyGeneration())
	}

	blocking = true
	result := make(chan frame, 1)
	go func() { result <- run() }()
	<-entered
	toggled := make(chan struct{})
	go func() {
		processor.SetAnonymizationEnabled(false)
		close(toggled)
	}()
	close(release)
	<-toggled
	item := <-result
	egress := newTestEgress(config.WireFormatJPEG, "out.flv")
	egress.InvalidateHeldFrame(processor.PrivacyGeneration())
	if egress.holdable(item.privacyGeneration) {
		t.Fatalf("frame processed across a setting change is holdable (generation %d)", item.privacyGeneration)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
