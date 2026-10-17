package media

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// pipelinedFakeStream은 테스트가 응답 시점을 정하는 AI 스트림이다. 보낸 프레임은
// 순서대로 쌓이고, release로 맨 앞 프레임의 결과를 하나씩 풀어 준다. 실패를 풀면
// 실제 스트림처럼 닫혀서 이미 보낸 나머지 프레임은 ai.ErrStreamReset을 받는다.
type sentAIFrame struct {
	data      string
	timestamp int64
}

type pipelinedFakeStream struct {
	mu        sync.Mutex
	queue     []sentAIFrame
	broken    bool
	discarded int
	sent      chan string
	release   chan error
	closed    chan struct{}
	closeOnce sync.Once
}

func newPipelinedFakeStream() *pipelinedFakeStream {
	return &pipelinedFakeStream{
		sent:    make(chan string, 16),
		release: make(chan error, 16),
		closed:  make(chan struct{}),
	}
}

func (f *pipelinedFakeStream) Process([]byte, int64, uint16, uint16, string) (*aiv1.ProcessedVideoChunk, error) {
	return nil, errors.New("pipelined stream must not use Process")
}

func (f *pipelinedFakeStream) Close() {
	f.closeOnce.Do(func() { close(f.closed) })
}

func (f *pipelinedFakeStream) Send(data []byte, timestamp int64, _, _ uint16, _ string) (ai.Pending, error) {
	f.mu.Lock()
	f.broken = false
	f.queue = append(f.queue, sentAIFrame{data: string(data), timestamp: timestamp})
	f.mu.Unlock()
	f.sent <- string(data)
	return ai.Pending{}, nil
}

func (f *pipelinedFakeStream) Recv(ai.Pending) (*aiv1.ProcessedVideoChunk, error) {
	f.mu.Lock()
	if f.broken {
		f.queue = f.queue[1:]
		f.mu.Unlock()
		return nil, ai.ErrStreamReset
	}
	f.mu.Unlock()
	select {
	case err := <-f.release:
		f.mu.Lock()
		sent := f.queue[0]
		f.queue = f.queue[1:]
		if err != nil {
			f.broken = true
		}
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &aiv1.ProcessedVideoChunk{Data: []byte("out-" + sent.data), Timestamp: sent.timestamp, StatusMessage: "success"}, nil
	case <-f.closed:
		f.mu.Lock()
		f.queue = f.queue[1:]
		f.mu.Unlock()
		return nil, errors.New("stream closed")
	}
}

func (f *pipelinedFakeStream) Discard(ai.Pending) {
	f.mu.Lock()
	f.discarded++
	f.mu.Unlock()
}

func newPipelinedProcessor(t *testing.T, stream *pipelinedFakeStream, registry *metrics.Registry) *Processor {
	t.Helper()
	processor, err := NewProcessor(config.PrivacyModeReal, 0, stream, registry, nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	processor.SetAIInflight(2)
	return processor
}

func runPipelinedTask(processor *Processor, data string) aiTask {
	return processor.beginAIInput(context.Background(), []byte(data), 0, 64, 48)
}

func waitSent(t *testing.T, stream *pipelinedFakeStream, want string) {
	t.Helper()
	select {
	case got := <-stream.sent:
		if got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("frame %q was not sent", want)
	}
}

func assertNotSent(t *testing.T, stream *pipelinedFakeStream) {
	t.Helper()
	select {
	case got := <-stream.sent:
		t.Fatalf("frame %q sent beyond the in-flight limit", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// 응답을 기다리는 동안 다음 프레임을 미리 보내되 상한을 넘지 않고, 결과는 보낸
// 순서대로 내보낸다.
func TestProcessImagesPipelinedSendsAheadInOrder(t *testing.T) {
	stream := newPipelinedFakeStream()
	registry := metrics.New()
	processor := newPipelinedProcessor(t, stream, registry)
	decoded := make(chan frame, 3)
	for _, data := range []string{"a", "b", "c"} {
		registry.AddQueue(1)
		decoded <- frame{data: []byte(data), width: 64, height: 48}
	}
	close(decoded)
	processed := make(chan frame, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		processImages(context.Background(), nil, processor, decoded, processed, NewEgressFanout(), registry, config.PrivacyModeReal, nil)
	}()

	waitSent(t, stream, "a")
	waitSent(t, stream, "b")
	assertNotSent(t, stream)
	stream.release <- nil
	waitSent(t, stream, "c")
	stream.release <- nil
	stream.release <- nil

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipelined processing did not finish")
	}
	for _, want := range []string{"out-a", "out-b", "out-c"} {
		item, ok := <-processed
		if !ok {
			t.Fatalf("processed closed before %q", want)
		}
		if string(item.data) != want {
			t.Fatalf("processed %q, want %q", item.data, want)
		}
	}
}

// 앞 프레임이 실패해 스트림이 닫히면 이미 보낸 뒤 프레임도 원본 대신 가린다.
// 잠금은 한 번만 건다.
func TestPipelinedAIFailureBlacksOutInFlightFrames(t *testing.T) {
	stream := newPipelinedFakeStream()
	registry := metrics.New()
	processor := newPipelinedProcessor(t, stream, registry)

	first := runPipelinedTask(processor, "a")
	second := runPipelinedTask(processor, "b")
	stream.release <- errors.New("worker crashed")

	for _, task := range []aiTask{first, second} {
		output, processed, err := processor.finishAIInput(task)
		if err != nil || !processed {
			t.Fatalf("finishAIInput() = processed %v, error %v; want fail-closed frame", processed, err)
		}
		if len(output) == 0 || string(output) == "a" || string(output) == "b" {
			t.Fatalf("finishAIInput() output = %q, want blackout frame", output)
		}
	}
	if !processor.FallbackActive() {
		t.Fatal("session did not latch after AI failure")
	}
	if got := metricValue(t, registry, `innolive_ai_fallback_latched_total{mode="real"}`); got != "1" {
		t.Fatalf("latched total = %s, want 1", got)
	}
}

// 응답을 기다리는 동안 입력 중단이 스트림을 닫으면 AI 실패로 잠그지 않고 중단 구간
// 프레임처럼 버린다.
func TestPipelinedSuspendDropsInFlightFrameWithoutLatch(t *testing.T) {
	stream := newPipelinedFakeStream()
	processor := newPipelinedProcessor(t, stream, metrics.New())

	task := runPipelinedTask(processor, "a")
	processor.SuspendAIInput()
	processor.ResumeAIInput()

	output, processed, err := processor.finishAIInput(task)
	if processed || err != nil || output != nil {
		t.Fatalf("finishAIInput() = %q, processed %v, error %v; want dropped frame", output, processed, err)
	}
	if processor.FallbackActive() {
		t.Fatal("suspend while a frame was in flight latched the session")
	}
}

// 잠긴 세션은 보내 둘 수 있어도 회복 시도 주기 사이에는 AI로 보내지 않는다.
func TestPipelinedLatchedSessionWaitsForProbeInterval(t *testing.T) {
	stream := newPipelinedFakeStream()
	processor := newPipelinedProcessor(t, stream, metrics.New())
	processor.fallback.Store(true)
	processor.lastProbeAt.Store(time.Now().UnixNano())

	task := runPipelinedTask(processor, "a")
	if task.pending != nil {
		t.Fatal("latched session sent a frame before the probe interval")
	}
	if len(task.output) == 0 || string(task.output) == "a" || task.err != nil {
		t.Fatalf("latched frame = %q, error %v; want blackout frame", task.output, task.err)
	}
}

func TestProcessorUsesSingleFrameAIByDefault(t *testing.T) {
	stream := newPipelinedFakeStream()
	processor, err := NewProcessor(config.PrivacyModeReal, 0, stream, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := processor.pipelinedAIInflight(); got != 1 {
		t.Fatalf("default in-flight = %d, want 1", got)
	}
	processor.SetAIInflight(MaxAIInflight + 3)
	if got := processor.pipelinedAIInflight(); got != MaxAIInflight {
		t.Fatalf("in-flight = %d, want cap %d", got, MaxAIInflight)
	}
}
