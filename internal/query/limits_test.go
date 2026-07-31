package query

import (
	"context"
	"testing"
)

func TestGateRefusesOnceTheQueueIsFull(t *testing.T) {
	g := newGate(1, 1) // 1 executing + 1 waiting = capacity 2

	r1, ok := g.acquire(context.Background())
	if !ok {
		t.Fatal("first acquire must succeed")
	}

	// Second call occupies the queue slot but blocks on the execution slot,
	// so run it in a goroutine and let the third prove refusal.
	started := make(chan struct{})
	go func() {
		close(started)
		if r2, ok := g.acquire(context.Background()); ok {
			r2()
		}
	}()
	<-started

	// Third caller: with capacity 2 already committed, this must be refused
	// rather than pile up another blocked goroutine on the recv path.
	for i := 0; i < 100; i++ {
		if _, ok := g.acquire(context.Background()); !ok {
			r1()
			return
		}
	}
	r1()
	t.Fatal("gate never refused; queue bound is not enforced")
}

func TestGateReleaseFreesTheSlot(t *testing.T) {
	g := newGate(1, 0)
	release, ok := g.acquire(context.Background())
	if !ok {
		t.Fatal("first acquire must succeed")
	}
	release()
	if _, ok := g.acquire(context.Background()); !ok {
		t.Fatal("a released slot must be reusable")
	}
}
