package supervised

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunWithRecover_CleanReturnDoesNotRestart(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		RunWithRecover(ctx, "clean", func(c context.Context) {
			atomic.AddInt32(&calls, 1)
			// Return immediately — no panic, not blocked on ctx.
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithRecover did not return after clean fn exit")
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("fn called %d times on clean return; want 1", got)
	}
}

func TestRunWithRecover_PanicRestartsWithBackoff(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())

	go RunWithRecover(ctx, "panic-test", func(c context.Context) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&calls) < 3 {
			panic("deliberate")
		}
		// On 3rd call, wait for ctx.Done so we can inspect calls cleanly.
		<-c.Done()
	})

	// Give it time to panic + restart at least twice (1s + 2s backoffs).
	time.Sleep(4 * time.Second)
	cancel()

	got := atomic.LoadInt32(&calls)
	if got < 3 {
		t.Errorf("fn called %d times after 4s of restarts; want >= 3", got)
	}
}

func TestRunWithRecover_CtxDoneStopsRestart(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		RunWithRecover(ctx, "ctx-done", func(c context.Context) {
			atomic.AddInt32(&calls, 1)
			panic("always")
		})
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(35 * time.Second):
		t.Fatal("RunWithRecover did not return after ctx cancel")
	}
}

func TestLivenessRegistry(t *testing.T) {
	// Heartbeat records; LastTick reads back.
	before := time.Now()
	Heartbeat("testsubsys-A")
	after := time.Now()

	last := LastTick("testsubsys-A")
	if last.Before(before) || last.After(after) {
		t.Errorf("LastTick=%v outside [%v, %v]", last, before, after)
	}

	// Unknown subsystem → zero time.
	if !LastTick("nonexistent-subsys").IsZero() {
		t.Errorf("LastTick for unknown subsystem should be zero")
	}

	// IsHealthy: fresh heartbeat → healthy; stale → not healthy.
	Heartbeat("testsubsys-B")
	healthy, stale := IsHealthy([]string{"testsubsys-A", "testsubsys-B"}, time.Minute)
	if !healthy || len(stale) != 0 {
		t.Errorf("expected healthy, got healthy=%v stale=%v", healthy, stale)
	}

	// Force staleness by asking for an impossibly tight threshold.
	time.Sleep(10 * time.Millisecond)
	healthy, stale = IsHealthy([]string{"testsubsys-A"}, time.Nanosecond)
	if healthy || len(stale) != 1 {
		t.Errorf("expected stale testsubsys-A, got healthy=%v stale=%v", healthy, stale)
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	Heartbeat("snap-test")
	s1 := Snapshot()
	// Mutating the returned map must not affect the registry.
	s1["snap-test"] = time.Time{}
	if LastTick("snap-test").IsZero() {
		t.Errorf("Snapshot returned a live reference; should be a copy")
	}
}
