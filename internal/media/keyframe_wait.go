package media

import (
	"log/slog"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// keyframeWait는 디코더가 첫 키프레임을 기다리는 동안 버리는 프레임을 세고
// 송신자에게 키프레임을 요청한다(#493). 파이프라인이 GOP 중간에서 시작하거나
// 해상도 변경으로 다시 시작하면(#283) 디코더는 키프레임부터만 띄울 수 있는데,
// WebRTC 송신자는 요청이 없으면 키프레임을 거의 보내지 않는다. 요청하지 않으면
// 그동안 처리 영상이 끊기고(실측 19.5초) 버린 프레임은 어떤 카운터에도 남지 않는다.
//
// nil이면 아무것도 하지 않으므로 피드백 채널이 없는 호출자는 nil을 넘긴다.
type keyframeWait struct {
	registry *metrics.Registry
	mode     string
	// request는 송신자에게 새 키프레임을 요청한다. nil이면 세기만 한다.
	request       func()
	minInterval   time.Duration
	now           func() time.Time
	lastRequestAt time.Time
	// 아래는 대기 종료 로그용이다. 디코드 고루틴 하나만 쓴다.
	discarded      int
	requests       int
	firstDiscardAt time.Time
}

func newKeyframeWait(registry *metrics.Registry, mode config.PrivacyMode, request func()) *keyframeWait {
	return &keyframeWait{
		registry:    registry,
		mode:        string(mode),
		request:     request,
		minInterval: keyframeRequestMinInterval,
		now:         time.Now,
	}
}

// discard는 키프레임을 기다리며 버린 프레임 하나를 센다. 첫 폐기에서 바로
// 키프레임을 요청하고, 계속 기다리면 손실 복구와 같은 간격으로 다시 요청한다.
// 송신자가 키프레임을 인코딩해 보낼 시간이 있어야 다음 요청이 의미 있다.
func (w *keyframeWait) discard() {
	if w == nil {
		return
	}
	if w.registry != nil {
		w.registry.IncKeyframeWaitDiscarded(w.mode)
	}
	now := w.now()
	if w.discarded == 0 {
		w.firstDiscardAt = now
	}
	w.discarded++
	if w.request == nil {
		return
	}
	if !w.lastRequestAt.IsZero() && now.Sub(w.lastRequestAt) < w.minInterval {
		return
	}
	w.lastRequestAt = now
	w.requests++
	w.request()
}

// end는 대기가 끝나면 한 번 부른다. 버린 프레임이 있었으면 폐기 수·요청 수·대기
// 시간을 남긴다. 긴 대기는 드물어서(재시작 3주 65회 중 1초 초과 3회) 카운터
// 합계만으로는 재요청이 대기를 줄였는지 가를 수 없으므로 건마다 남긴다.
// keyframeReceived가 false면 키프레임 없이 입력이 끝났거나 취소된 것이다.
func (w *keyframeWait) end(logger *slog.Logger, codec VideoCodec, keyframeReceived bool) {
	if w == nil || w.discarded == 0 {
		return
	}
	logger.Info("decoder keyframe wait ended",
		"codec", codec,
		"keyframe_received", keyframeReceived,
		"discarded_frames", w.discarded,
		"keyframe_requests", w.requests,
		"waited_ms", w.now().Sub(w.firstDiscardAt).Milliseconds())
}
