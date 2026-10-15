package bus

import (
	"context"
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
		return Event{}
	}
}

func TestPublishSubscribe(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	b.Publish(TypeWorkItemCreated, map[string]any{"key": "FT-1"})
	ev := recv(t, ch)
	if ev.Seq != 1 || ev.Type != TypeWorkItemCreated {
		t.Fatalf("ev=%+v", ev)
	}
}

func TestReplay(t *testing.T) {
	b := New()
	for i := 0; i < 5; i++ {
		b.Publish(TypeWorkItemCreated, i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 3)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	for want := uint64(3); want <= 5; want++ {
		if ev := recv(t, ch); ev.Seq != want {
			t.Fatalf("got seq %d, want %d", ev.Seq, want)
		}
	}
	// Live events continue after the replay.
	b.Publish(TypeWorkItemCreated, "live")
	if ev := recv(t, ch); ev.Seq != 6 {
		t.Fatalf("live seq=%d", ev.Seq)
	}
}

func TestCursorTooOld(t *testing.T) {
	b := New()
	for i := 0; i < 2*ringSize+10; i++ {
		b.Publish(TypeWorkItemCreated, i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := b.Subscribe(ctx, 1); err != ErrCursorTooOld {
		t.Fatalf("got %v, want ErrCursorTooOld", err)
	}
	// A fresh cursor within the ring works.
	oldest := b.ring[0].Seq
	if _, err := b.Subscribe(ctx, oldest); err != nil {
		t.Fatalf("oldest cursor: %v", err)
	}
}

func TestSlowConsumerDisconnect(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Flood without reading: the subscriber must be cut off, never blocking us.
	for i := 0; i < subBuffer+10; i++ {
		b.Publish(TypeWorkItemCreated, i)
	}
	// Drain what fits, then observe the close.
	n := 0
	closed := false
	timeout := time.After(2 * time.Second)
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			} else {
				n++
			}
		case <-timeout:
			t.Fatal("channel neither drained nor closed")
		}
	}
	if n != subBuffer {
		t.Fatalf("drained %d, want %d before disconnect", n, subBuffer)
	}
}

func TestConcurrentPublish(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n/8; j++ {
				b.Publish(TypeWorkItemCreated, nil)
			}
		}()
	}
	wg.Wait()
	seen := map[uint64]bool{}
	for i := 0; i < n; i++ {
		ev := recv(t, ch)
		if seen[ev.Seq] {
			t.Fatalf("duplicate seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
	}
	if b.CurrentSeq() != n {
		t.Fatalf("seq=%d, want %d", b.CurrentSeq(), n)
	}
}
