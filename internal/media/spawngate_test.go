package media

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNilSpawnGateIsUnlimited(t *testing.T) {
	var gate *SpawnGate
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatalf("nil gate Acquire() error = %v", err)
	}
	gate.Release()
}

func TestSpawnGateZeroSizeIsUnlimited(t *testing.T) {
	if gate := NewSpawnGate(0); gate != nil {
		t.Fatalf("NewSpawnGate(0) = %v, want nil", gate)
	}
}

func TestSpawnGateBlocksAtCapacityAndUnblocksOnRelease(t *testing.T) {
	gate := NewSpawnGate(2)
	ctx := context.Background()
	if err := gate.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := gate.Acquire(ctx); err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := gate.Acquire(ctx); err == nil {
			close(acquired)
		}
	}()
	select {
	case <-acquired:
		t.Fatal("third Acquire() should block at capacity 2")
	case <-time.After(50 * time.Millisecond):
	}

	gate.Release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("Acquire() did not unblock after Release()")
	}
}

func TestSpawnGateAcquireRespectsContextCancellation(t *testing.T) {
	gate := NewSpawnGate(1)
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- gate.Acquire(ctx) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire() did not return after context cancellation")
	}
}
