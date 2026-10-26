package ai

import (
	"testing"
	"time"
)

func TestVODLiveAcquireRemainsImmediate(t *testing.T) {
	p, err := NewPool([]string{"worker-a:5000", "worker-b:5000"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	mapping := map[string]string{"worker-a:5000": "GPU-0", "worker-b:5000": "GPU-0"}
	l := p.ReserveVOD(mapping, map[string]int64{"GPU-0": 8192}, 4096, 2)
	if l == nil {
		t.Fatal("no VOD lease")
	}
	defer l.Release()
	// No consumer acknowledges preemption. Live must still return immediately,
	// with exactly the same round-robin choice and active count as an empty pool.
	done := make(chan struct{})
	go func() {
		c, release := p.Acquire(false)
		if c.Address() != "worker-a:5000" {
			t.Errorf("live ranking changed: %s", c.Address())
		}
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("live waited for VOD")
	}
	select {
	case <-l.Preempt:
	default:
		t.Fatal("missing preemption")
	}
	c, release := p.Acquire(false)
	defer release()
	if c.Address() != "worker-b:5000" {
		t.Fatal("round robin changed")
	}
}
func TestVODGPUAdmissionAndLiveMovePreemption(t *testing.T) {
	p, _ := NewPool([]string{"a:1", "b:1", "c:1"}, time.Second)
	defer p.Close()
	mapping := map[string]string{"a:1": "GPU-0", "b:1": "GPU-0", "c:1": "GPU-1"}
	free := map[string]int64{"GPU-0": 8192, "GPU-1": 8192}
	first := p.ReserveVOD(mapping, free, 4096, 2)
	if first == nil {
		t.Fatal("first")
	}
	defer first.Release()
	second := p.ReserveVOD(mapping, free, 4096, 2)
	if second == nil || second.GPU == first.GPU {
		t.Fatal("per-GPU cap")
	}
	defer second.Release()
	if p.ReserveVOD(mapping, free, 4096, 2) != nil {
		t.Fatal("global cap")
	}
	_, release, ok := p.AcquireMoveTarget(false, "c:1", "b:1")
	if !ok {
		t.Fatal("move rejected")
	}
	defer release()
	select {
	case <-first.Preempt:
	default:
		t.Fatal("same GPU sibling did not preempt")
	}
	first.Release()
	if p.ReserveVOD(mapping, free, 4096, 3) != nil {
		t.Fatal("VOD admitted beside live")
	}
}
func TestVODUnknownMemoryAndDrainingFailClosed(t *testing.T) {
	p, _ := NewPool([]string{"a:1"}, time.Second)
	defer p.Close()
	mapping := map[string]string{"a:1": "GPU-0"}
	for _, free := range []map[string]int64{nil, {"GPU-0": 4095}} {
		if p.ReserveVOD(mapping, free, 4096, 1) != nil {
			t.Fatal("unsafe memory admission")
		}
	}
	l := p.ReserveVOD(mapping, map[string]int64{"GPU-0": 4096}, 4096, 1)
	if l == nil {
		t.Fatal("memory boundary")
	}
	defer l.Release()
	if err := p.SetDraining("a:1", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Preempt:
	default:
		t.Fatal("drain did not preempt")
	}
}

func TestVODUnmappedReplacementStopsAdmission(t *testing.T) {
	p, _ := NewPool([]string{"a:1"}, time.Second)
	defer p.Close()
	mapping := map[string]string{"a:1": "GPU-0"}
	free := map[string]int64{"GPU-0": 8192}
	l := p.ReserveVOD(mapping, free, 4096, 1)
	if l == nil {
		t.Fatal("lease")
	}
	defer l.Release()
	if _, err := p.AddTarget("b:1", false); err != nil {
		t.Fatal(err)
	}
	_ = p.SetDraining("b:1", false)
	_, release, ok := p.AcquireMoveTarget(false, "a:1", "b:1")
	if !ok {
		t.Fatal("live replacement blocked")
	}
	defer release()
	select {
	case <-l.Preempt:
	default:
		t.Fatal("unknown physical GPU did not yield")
	}
	l.Release()
	if p.ReserveVOD(mapping, free, 4096, 1) != nil {
		t.Fatal("incomplete GPU topology admitted")
	}
}
