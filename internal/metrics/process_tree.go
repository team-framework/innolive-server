package metrics

import "sync"

type childProcessUsage struct {
	cpuSeconds float64
	rssBytes   uint64
}

type processTreeTracker struct {
	mu                  sync.Mutex
	active              map[int]childProcessUsage
	completedCPUSeconds float64
}

func newProcessTreeTracker() processTreeTracker {
	return processTreeTracker{active: make(map[int]childProcessUsage)}
}

func (r *Registry) RegisterChildProcess(pid int) {
	if pid <= 0 {
		return
	}
	r.processTree.mu.Lock()
	if r.processTree.active == nil {
		r.processTree.active = make(map[int]childProcessUsage)
	}
	r.processTree.active[pid] = childProcessUsage{}
	r.processTree.mu.Unlock()
}

func (r *Registry) CompleteChildProcess(pid int, cpuSeconds float64) {
	if pid <= 0 {
		return
	}
	r.processTree.mu.Lock()
	usage, ok := r.processTree.active[pid]
	if !ok {
		r.processTree.mu.Unlock()
		return
	}
	if cpuSeconds < usage.cpuSeconds {
		cpuSeconds = usage.cpuSeconds
	}
	r.processTree.completedCPUSeconds += cpuSeconds
	delete(r.processTree.active, pid)
	r.processTree.mu.Unlock()
}

func (r *Registry) processTreeStats(selfCPUSeconds float64, selfRSSBytes uint64) (float64, uint64) {
	r.processTree.mu.Lock()
	defer r.processTree.mu.Unlock()

	cpuSeconds := selfCPUSeconds + r.processTree.completedCPUSeconds
	rssBytes := selfRSSBytes
	for pid, usage := range r.processTree.active {
		childCPUSeconds, childRSSBytes, ok := readChildProcessStats(pid)
		if ok {
			if childCPUSeconds > usage.cpuSeconds {
				usage.cpuSeconds = childCPUSeconds
			}
			usage.rssBytes = childRSSBytes
			r.processTree.active[pid] = usage
		}
		cpuSeconds += usage.cpuSeconds
		rssBytes += usage.rssBytes
	}
	return cpuSeconds, rssBytes
}
