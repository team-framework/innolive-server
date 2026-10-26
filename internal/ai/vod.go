package ai

import (
	"sync"
)

type VODLease struct {
	Target  string
	GPU     string
	Preempt <-chan struct{}
	signal  chan struct{}
	once    sync.Once
	pool    *Pool
}

func (l *VODLease) interrupt() { l.once.Do(func() { close(l.signal) }) }
func (l *VODLease) Release() {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.pool.vod[l.GPU] == l {
		delete(l.pool.vod, l.GPU)
	}
}

// ReserveVOD and live claim use the same mutex. VOD never changes live counters
// or ranking. Preemption is a channel close only: no RPC or I/O under pool.mu.
func (p *Pool) ReserveVOD(mapping map[string]string, freeMiB map[string]int64, minFree int64, limit int) *VODLease {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		if mapping[c.Address()] == "" {
			return nil
		}
	}
	if len(p.vod) >= limit {
		return nil
	}
	for i, c := range p.clients {
		gpu := mapping[c.Address()]
		free, known := freeMiB[gpu]
		if gpu == "" || !known || free < minFree || p.vod[gpu] != nil || p.active[i] > 0 || p.unready[c.Address()] || p.draining[c.Address()] || p.faceSwap[c.Address()] {
			continue
		}
		busy := false
		for k, other := range p.clients {
			if mapping[other.Address()] == gpu && p.active[k] > 0 {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		ch := make(chan struct{})
		l := &VODLease{Target: c.Address(), GPU: gpu, signal: ch, Preempt: ch, pool: p}
		if p.vod == nil {
			p.vod = map[string]*VODLease{}
		}
		p.vod[gpu] = l
		// Copy mapping so callers cannot race with the live hot path.
		p.vodGPUs = make(map[string]string, len(mapping))
		for target, id := range mapping {
			p.vodGPUs[target] = id
		}
		return l
	}
	return nil
}
func (p *Pool) preemptVODLocked(target string) {
	if p.vodGPUs[target] == "" {
		for _, l := range p.vod {
			l.interrupt()
		}
		return
	}
	if l := p.vod[p.vodGPUs[target]]; l != nil {
		l.interrupt()
	}
}
