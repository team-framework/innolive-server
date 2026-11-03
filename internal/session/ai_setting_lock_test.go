package session

import (
	"context"
	"sync"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"
)

// blockingAIStream은 release가 닫힐 때까지 응답하지 않는 AI 워커다.
type blockingAIStream struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingAIStream) Process([]byte, int64, uint16, uint16, string, *aiv1.MosaicConfig) (*aiv1.ProcessedVideoChunk, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil, context.Canceled
}

func (*blockingAIStream) Close() {}

// 익명화 설정 변경은 진행 중인 AI 호출이 끝나기를 기다린다(최대 AI 타임아웃).
// 그동안 세션 잠금을 쥐고 있으면 같은 세션의 상태 조회와 다른 API가 모두 같이
// 멈춘다(P1-1).
func TestAISettingChangeDoesNotHoldSessionLockWhileAIIsBusy(t *testing.T) {
	manager := newTestManager(t, 0)
	live, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	ai := &blockingAIStream{entered: make(chan struct{}), release: make(chan struct{})}
	releaseAI := sync.OnceFunc(func() { close(ai.release) })
	t.Cleanup(releaseAI)
	processor, err := media.NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 3)
	if err != nil {
		t.Fatal(err)
	}
	live.mu.Lock()
	live.processor = processor
	live.mu.Unlock()

	go func() { _, _ = processor.Process(context.Background(), []byte{0xff, 0xd8, 0xff, 0xd9}, 0, 16, 16) }()
	select {
	case <-ai.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("AI call did not start")
	}

	changed := make(chan error, 1)
	go func() {
		_, err := manager.SetAnonymizationEnabled(live.ID, false)
		changed <- err
	}()
	time.Sleep(50 * time.Millisecond)

	read := make(chan struct{})
	go func() {
		_ = live.Response()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("session state was blocked behind the in-flight AI call")
	}

	releaseAI()
	select {
	case err := <-changed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("setting change did not finish after the AI call returned")
	}
	if processor.AnonymizationEnabled() {
		t.Fatal("processor still anonymizing after the change")
	}
}
