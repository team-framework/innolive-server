package server

import (
	"sync"
	"time"
)

// ownedBroadcastTTL은 InnoLive가 만든 방송 id를 기억하는 시간이다. 준비 시각부터
// 세므로 가장 긴 1회 방송(Plasma 12시간)이 끝나고 autoStop이 닫는 1분까지 덮어야
// 한다 — 짧으면 긴 방송 직후 다시 준비할 때 우리 방송을 남의 방송으로 센다.
const ownedBroadcastTTL = 24 * time.Hour

// ownedBroadcasts는 InnoLive가 만든 플랫폼 방송 id다(#361). 채널의 라이브 방송을
// 셀 때 우리 방송을 다른 도구의 방송으로 잘못 세지 않게 한다.
type ownedBroadcasts struct {
	mu  sync.Mutex
	ids map[string]time.Time
}

func (o *ownedBroadcasts) add(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	if o.ids == nil {
		o.ids = map[string]time.Time{}
	}
	for known, at := range o.ids {
		if now.Sub(at) > ownedBroadcastTTL {
			delete(o.ids, known)
		}
	}
	o.ids[id] = now
}

func (o *ownedBroadcasts) contains(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	at, ok := o.ids[id]
	return ok && time.Since(at) <= ownedBroadcastTTL
}
