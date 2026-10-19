package media

import (
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
	if w.request == nil {
		return
	}
	now := w.now()
	if !w.lastRequestAt.IsZero() && now.Sub(w.lastRequestAt) < w.minInterval {
		return
	}
	w.lastRequestAt = now
	w.request()
}
