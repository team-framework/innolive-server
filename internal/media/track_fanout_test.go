package media

import (
	"context"
	"testing"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func newFanoutSink(registry *metrics.Registry) *RTMPEgress {
	return &RTMPEgress{input: make(chan frame, 4), metrics: registry}
}

// runOneFrame은 카메라 프레임 하나를 처리 루프에 흘린다.
func runOneFrame(t *testing.T, fanout *EgressFanout, registry *metrics.Registry) {
	t.Helper()
	processor, err := NewProcessor(
		config.PrivacyModeReal,
		0,
		&fakeAIStream{process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
			return &aiv1.ProcessedVideoChunk{Data: []byte("output"), Timestamp: timestamp, StatusMessage: "success"}, nil
		}},
		registry,
		nil,
		config.WireFormatJPEG,
		config.FailurePolicyFreeze,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	decoded := make(chan frame, 1)
	decoded <- frame{data: []byte("camera-frame"), width: 640, height: 480}
	close(decoded)
	processed := make(chan frame, 1)
	processImages(context.Background(), nil, processor, decoded, processed, fanout, registry, config.PrivacyModeReal, nil)
}

// 처리된 프레임은 설치된 모든 대상으로 간다 — 동시 송출의 전제다(#232).
func TestEgressFanoutDeliversToEveryTarget(t *testing.T) {
	registry := metrics.New()
	fanout := NewEgressFanout()
	youtube, chzzk := newFanoutSink(registry), newFanoutSink(registry)
	fanout.Add("youtube", youtube)
	fanout.Add("chzzk", chzzk)

	runOneFrame(t, fanout, registry)

	if len(youtube.input) != 1 {
		t.Fatalf("youtube received %d frames, want 1", len(youtube.input))
	}
	if len(chzzk.input) != 1 {
		t.Fatalf("chzzk received %d frames, want 1", len(chzzk.input))
	}
}

// 한 대상을 떼도 나머지는 계속 받는다 — 한쪽 종료가 다른 쪽을 끊으면 안 된다.
func TestEgressFanoutRemoveKeepsOtherTargets(t *testing.T) {
	registry := metrics.New()
	fanout := NewEgressFanout()
	youtube, chzzk := newFanoutSink(registry), newFanoutSink(registry)
	fanout.Add("youtube", youtube)
	fanout.Add("chzzk", chzzk)
	fanout.Remove("chzzk")

	runOneFrame(t, fanout, registry)

	if len(youtube.input) != 1 {
		t.Fatalf("youtube received %d frames, want 1", len(youtube.input))
	}
	if len(chzzk.input) != 0 {
		t.Fatalf("removed chzzk received %d frames, want 0", len(chzzk.input))
	}
}

// RemoveAll은 세션 종료 경로다. 이후 프레임은 아무 대상으로도 가지 않는다.
func TestEgressFanoutRemoveAllStopsDelivery(t *testing.T) {
	registry := metrics.New()
	fanout := NewEgressFanout()
	sink := newFanoutSink(registry)
	fanout.Add("youtube", sink)
	fanout.RemoveAll()

	runOneFrame(t, fanout, registry)

	if len(sink.input) != 0 {
		t.Fatalf("sink received %d frames after RemoveAll, want 0", len(sink.input))
	}
}

// 대상 순서는 설치 순서가 아니라 키 순서로 고정한다. map 순회 순서가 프레임마다
// 달라지면 대상 간 전달 순서가 흔들려 시작 시점 차이(#234)를 잴 수 없다.
func TestEgressFanoutSinkOrderIsStable(t *testing.T) {
	registry := metrics.New()
	fanout := NewEgressFanout()
	youtube, chzzk := newFanoutSink(registry), newFanoutSink(registry)
	fanout.Add("youtube", youtube)
	fanout.Add("chzzk", chzzk)

	for range 3 {
		sinks := fanout.Sinks()
		if len(sinks) != 2 || sinks[0] != chzzk || sinks[1] != youtube {
			t.Fatalf("Sinks() = %v, want [chzzk %p, youtube %p]", sinks, chzzk, youtube)
		}
	}
}
