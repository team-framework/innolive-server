package media

import "context"

// SpawnGate는 동시에 fork할 수 있는 FFmpeg 프로세스 수를 제한한다. N개 세션이 한꺼번에
// 몰리면 fork/exec가 거의 동시에 2N번 터지는데, 게이트가 이를 짧은 경사로로 바꾼다.
// 토큰은 프로세스 시작 동안만 쥐고 프로세스 수명 동안 쥐지 않으므로 정상 상태
// 처리량에는 영향이 없다.
//
// nil *SpawnGate는 유효하며 제한 없음을 뜻한다.
type SpawnGate struct {
	tokens chan struct{}
}

func NewSpawnGate(size int) *SpawnGate {
	if size <= 0 {
		return nil
	}
	return &SpawnGate{tokens: make(chan struct{}, size)}
}

func (g *SpawnGate) Acquire(ctx context.Context) error {
	if g == nil {
		return nil
	}
	select {
	case g.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *SpawnGate) Release() {
	if g == nil {
		return
	}
	<-g.tokens
}
