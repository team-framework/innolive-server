package media

import "context"

// SpawnGate bounds how many FFmpeg processes may be forked at the same
// moment. A cold spike of N simultaneous sessions otherwise turns into 2N
// near-simultaneous fork/exec bursts; the gate converts that into a short
// ramp. The token is held only across process start, never for the process
// lifetime, so steady-state throughput is unaffected.
//
// A nil *SpawnGate is valid and means unlimited.
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
