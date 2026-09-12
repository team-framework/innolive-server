package server

import (
	"context"
	"testing"
	"time"
)

func TestDecodeGateNilUnlimited(t *testing.T) {
	var g *decodeGate // size <= 0 yields nil
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("nil gate acquire: %v", err)
	}
	g.release() // must be a no-op, not a panic
}

func TestDecodeGateBoundsConcurrency(t *testing.T) {
	g := newDecodeGate(1)

	// First acquire takes the only token.
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Second acquire must block while the gate is saturated, then fail when its
	// context is canceled rather than proceeding past the limit.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx); err == nil {
		t.Fatal("expected acquire to fail while gate is saturated")
	}

	// After releasing, a fresh acquire succeeds again.
	g.release()
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	g.release()
}
