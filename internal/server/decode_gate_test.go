package server

import (
	"context"
	"testing"
	"time"
)

func TestDecodeGateNilUnlimited(t *testing.T) {
	var g *decodeGate // size <= 0이면 nil
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("nil gate acquire: %v", err)
	}
	g.release() // 패닉이 아니라 아무 일도 없어야 한다
}

func TestDecodeGateBoundsConcurrency(t *testing.T) {
	g := newDecodeGate(1)

	// 첫 acquire가 하나뿐인 토큰을 가져간다.
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// 두 번째 acquire는 게이트가 가득한 동안 막히고, 한계를 넘어 진행하지 않고
	// 컨텍스트가 취소되면 실패해야 한다.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx); err == nil {
		t.Fatal("expected acquire to fail while gate is saturated")
	}

	// 반납하면 새 acquire가 다시 성공한다.
	g.release()
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	g.release()
}
