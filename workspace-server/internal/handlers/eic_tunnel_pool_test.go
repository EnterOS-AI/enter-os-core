package handlers

// eic_tunnel_pool_test.go — tests for the refcounted EIC tunnel pool
// added in core#11. Stubs poolSetupTunnel with a counter so the
// tests don't fork ssh-keygen / aws subprocesses.
//
// Per memory feedback_assert_exact_not_substring: each test pins
// exact expected counts (not "at least N") so a regression that
// silently double-sets-up surfaces here.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withPoolSetupStub swaps poolSetupTunnel for a counting fake that
// returns a sentinel session and a cleanup func that records its
// invocation. Restores on test cleanup.
//
// setupSignal blocks each setup until released — for concurrent-
// acquire tests where we want to gate setup completion.
func withPoolSetupStub(t *testing.T) (
	setupCount *int64, cleanupCount *int64, restore func(), unblock func()) {
	t.Helper()
	prev := poolSetupTunnel
	prevTTL := poolTTL
	prevJanitor := poolJanitorInterval

	var sc, cc int64
	setupCount, cleanupCount = &sc, &cc

	gate := make(chan struct{}, 1)
	gate <- struct{}{} // allow the first setup through immediately
	unblock = func() { gate <- struct{}{} }

	poolSetupTunnel = func(ctx context.Context, instanceID string) (
		eicSSHSession, func(), error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return eicSSHSession{}, nil, ctx.Err()
		}
		atomic.AddInt64(&sc, 1)
		sess := eicSSHSession{
			instanceID: instanceID,
			osUser:     "ubuntu",
			localPort:  10000 + int(atomic.LoadInt64(&sc)),
			keyPath:    "/tmp/molecule-eic-test-" + instanceID,
		}
		cleanup := func() { atomic.AddInt64(&cc, 1) }
		return sess, cleanup, nil
	}

	restore = func() {
		poolSetupTunnel = prev
		poolTTL = prevTTL
		poolJanitorInterval = prevJanitor
	}
	t.Cleanup(restore)
	return
}

// freshPool returns an isolated pool (NOT the global) so tests run
// independently. Stops the janitor on cleanup.
func freshPool(t *testing.T) *eicTunnelPool {
	t.Helper()
	p := newEICTunnelPool()
	t.Cleanup(p.stop)
	return p
}

// TestEICTunnelPool_FourOpsAmortise pins the core invariant: four
// sequential acquire/release cycles on the same instanceID share
// ONE underlying tunnel setup. Mutation: delete the cache hit branch
// in acquire() → setupCount goes 1 → 4 → test fails.
func TestEICTunnelPool_FourOpsAmortise(t *testing.T) {
	setupCount, cleanupCount, _, _ := withPoolSetupStub(t)
	// Refill gate after each setup so concurrent stubs aren't blocked
	// (we want every test to be able to set up if it needs to).
	t.Cleanup(func() { /* no-op; defer is enough */ })
	poolTTL = 50 * time.Second
	pool := freshPool(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		sess, done, err := pool.acquire(ctx, "i-test-1")
		if err != nil {
			t.Fatalf("op %d: acquire: %v", i, err)
		}
		if sess.instanceID != "i-test-1" {
			t.Fatalf("op %d: session has wrong instanceID: %q", i, sess.instanceID)
		}
		done(false)
	}

	if got := atomic.LoadInt64(setupCount); got != 1 {
		t.Errorf("expected exactly 1 tunnel setup across 4 ops, got %d", got)
	}
	if got := atomic.LoadInt64(cleanupCount); got != 0 {
		t.Errorf("expected 0 cleanups while entry is hot (TTL=50s), got %d", got)
	}
}

// TestEICTunnelPool_DifferentInstancesDoNotShare pins that two
// different instanceIDs each get their own tunnel — the pool is
// keyed on instanceID, not a single global slot.
func TestEICTunnelPool_DifferentInstancesDoNotShare(t *testing.T) {
	setupCount, _, _, unblock := withPoolSetupStub(t)
	poolTTL = 50 * time.Second
	pool := freshPool(t)
	ctx := context.Background()

	// First instance setup uses the initial gate slot.
	_, doneA, err := pool.acquire(ctx, "i-a")
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	doneA(false)

	// Second instance needs a new slot through the gate.
	unblock()
	_, doneB, err := pool.acquire(ctx, "i-b")
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	doneB(false)

	if got := atomic.LoadInt64(setupCount); got != 2 {
		t.Errorf("expected 2 setups (one per instance), got %d", got)
	}
}

// TestEICTunnelPool_TTLEviction: a short TTL forces the second op
// to build a fresh tunnel after the first expires.
func TestEICTunnelPool_TTLEviction(t *testing.T) {
	setupCount, cleanupCount, _, unblock := withPoolSetupStub(t)
	poolTTL = 50 * time.Millisecond
	poolJanitorInterval = 1 * time.Second // keep janitor away
	pool := freshPool(t)
	ctx := context.Background()

	_, done, err := pool.acquire(ctx, "i-ttl")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	done(false)

	time.Sleep(80 * time.Millisecond) // past TTL

	unblock() // allow next setup
	_, done, err = pool.acquire(ctx, "i-ttl")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	done(false)

	if got := atomic.LoadInt64(setupCount); got != 2 {
		t.Errorf("expected 2 setups (TTL eviction between), got %d", got)
	}
	// First entry should have been cleaned up when the second
	// acquire evicted it on the slow path. Cleanup runs in a
	// goroutine; poll briefly for it to land.
	deadline := time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt64(cleanupCount) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(cleanupCount); got < 1 {
		t.Errorf("expected ≥1 cleanup (first entry evicted), got %d", got)
	}
}

// TestEICTunnelPool_FailureInvalidates pins the poison-on-fault
// behavior — fn returning a tunnel-fatal error marks the entry
// unusable so the next acquire builds fresh.
func TestEICTunnelPool_FailureInvalidates(t *testing.T) {
	setupCount, _, _, unblock := withPoolSetupStub(t)
	poolTTL = 50 * time.Second
	pool := freshPool(t)
	ctx := context.Background()

	_, done, err := pool.acquire(ctx, "i-fault")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	done(true) // signal poison

	unblock() // let the next setup through
	_, done, err = pool.acquire(ctx, "i-fault")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	done(false)

	if got := atomic.LoadInt64(setupCount); got != 2 {
		t.Errorf("expected 2 setups (poison forced rebuild), got %d", got)
	}
}

// TestEICTunnelPool_ConcurrentAcquireSingleSetup pins that N
// concurrent acquires for the same instanceID before any release
// only trigger ONE tunnel setup — the rest wait via pendingSetups.
//
// Without this guard each concurrent acquire would spawn its own
// tunnel and the loser-cleanup would still leak refcount. Mutation:
// delete the pendingSetups gate → setupCount goes 1 → N → fails.
func TestEICTunnelPool_ConcurrentAcquireSingleSetup(t *testing.T) {
	setupCount, _, _, _ := withPoolSetupStub(t)
	// Pause setup so all goroutines pile into the pending slot.
	prev := poolSetupTunnel
	gate := make(chan struct{})
	poolSetupTunnel = func(ctx context.Context, instanceID string) (
		eicSSHSession, func(), error) {
		<-gate
		atomic.AddInt64(setupCount, 1)
		return eicSSHSession{instanceID: instanceID}, func() {}, nil
	}
	t.Cleanup(func() { poolSetupTunnel = prev })

	poolTTL = 50 * time.Second
	pool := freshPool(t)
	ctx := context.Background()

	const N = 8
	type result struct {
		done func(bool)
		err  error
	}
	results := make(chan result, N)
	var startWg sync.WaitGroup
	startWg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			startWg.Done()
			_, done, err := pool.acquire(ctx, "i-concurrent")
			results <- result{done, err}
		}()
	}
	startWg.Wait()
	// give all N goroutines time to enter pool.acquire
	time.Sleep(20 * time.Millisecond)
	close(gate)

	for i := 0; i < N; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("acquire %d: %v", i, r.err)
		}
		r.done(false)
	}

	if got := atomic.LoadInt64(setupCount); got != 1 {
		t.Errorf("expected 1 setup across %d concurrent acquires, got %d", N, got)
	}
}

// TestEICTunnelPool_TTLZeroDisablesPooling pins the escape hatch:
// poolTTL=0 means every acquire goes straight through to setup +
// cleanup, no entry kept. Useful for tests / opt-out.
func TestEICTunnelPool_TTLZeroDisablesPooling(t *testing.T) {
	setupCount, cleanupCount, _, unblock := withPoolSetupStub(t)
	poolTTL = 0
	pool := freshPool(t)
	ctx := context.Background()

	_, done, err := pool.acquire(ctx, "i-ttlzero")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	done(false)

	unblock()
	_, done, err = pool.acquire(ctx, "i-ttlzero")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	done(false)

	if got := atomic.LoadInt64(setupCount); got != 2 {
		t.Errorf("expected 2 setups with TTL=0 (pool disabled), got %d", got)
	}
	if got := atomic.LoadInt64(cleanupCount); got != 2 {
		t.Errorf("expected 2 cleanups with TTL=0 (each release closes), got %d", got)
	}
}

// TestEICTunnelPool_LRUEvictionAtCap pins the LRU defence: when the
// pool reaches poolMaxEntries, a new acquire for an unseen
// instanceID evicts the LRU idle entry instead of growing unbounded.
func TestEICTunnelPool_LRUEvictionAtCap(t *testing.T) {
	setupCount, cleanupCount, _, _ := withPoolSetupStub(t)
	prev := poolMaxEntries
	poolMaxEntries = 2
	t.Cleanup(func() { poolMaxEntries = prev })
	poolTTL = 50 * time.Second

	// Replace stub with one that doesn't gate so we can fill quickly.
	poolSetupTunnel = func(ctx context.Context, instanceID string) (
		eicSSHSession, func(), error) {
		atomic.AddInt64(setupCount, 1)
		return eicSSHSession{instanceID: instanceID}, func() {
			atomic.AddInt64(cleanupCount, 1)
		}, nil
	}

	pool := freshPool(t)
	ctx := context.Background()

	for _, id := range []string{"i-1", "i-2"} {
		_, done, err := pool.acquire(ctx, id)
		if err != nil {
			t.Fatalf("acquire %s: %v", id, err)
		}
		done(false)
	}
	// Both entries idle, pool at cap.
	_, done, err := pool.acquire(ctx, "i-3")
	if err != nil {
		t.Fatalf("acquire i-3: %v", err)
	}
	done(false)

	// Wait for the goroutine'd cleanup of the evicted entry.
	deadline := time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt64(cleanupCount) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := atomic.LoadInt64(setupCount); got != 3 {
		t.Errorf("expected 3 setups (one per unique instance), got %d", got)
	}
	if got := atomic.LoadInt64(cleanupCount); got < 1 {
		t.Errorf("expected ≥1 cleanup (LRU eviction), got %d", got)
	}
}

// TestEICTunnelPool_PoisonedClassification pins the heuristic that
// distinguishes tunnel-fatal errors (poison the entry) from
// app-level errors (file not found, validation) that should NOT
// invalidate the tunnel.
func TestEICTunnelPool_PoisonedClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"file not found", errors.New("os: file does not exist"), false},
		{"validation", errors.New("invalid path: must be relative"), false},
		{"connection refused", errors.New("ssh: connect to host: connection refused"), true},
		{"connection refused upper", errors.New("Connection Refused"), true},
		{"broken pipe", errors.New("write tunnel: broken pipe"), true},
		{"permission denied", errors.New("Permission denied (publickey)"), true},
		{"auth failed", errors.New("Authentication failed"), true},
		{"connection reset", errors.New("Connection reset by peer"), true},
		{"port forward", errors.New("port forwarding failed"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fnErrIndicatesTunnelFault(tc.err)
			if got != tc.want {
				t.Errorf("fnErrIndicatesTunnelFault(%v) = %v, want %v",
					tc.err, got, tc.want)
			}
		})
	}
}

// TestEICTunnelPool_RefcountBlocksEviction pins that an entry past
// TTL is NOT evicted while a caller still holds it — preventing
// use-after-free in the holder.
func TestEICTunnelPool_RefcountBlocksEviction(t *testing.T) {
	setupCount, cleanupCount, _, _ := withPoolSetupStub(t)
	poolTTL = 30 * time.Millisecond
	poolJanitorInterval = 5 * time.Millisecond
	pool := freshPool(t)
	ctx := context.Background()

	_, done, err := pool.acquire(ctx, "i-hold")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Sleep past TTL while holding the session. Janitor sweeps
	// every 5ms but must skip our entry because refcount=1.
	time.Sleep(80 * time.Millisecond)

	if got := atomic.LoadInt64(cleanupCount); got != 0 {
		t.Errorf("expected 0 cleanups while holder is active, got %d", got)
	}

	done(false)
	// Now refcount=0 and entry is past TTL; releaser triggers cleanup.
	deadline := time.Now().Add(200 * time.Millisecond)
	for atomic.LoadInt64(cleanupCount) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(cleanupCount); got != 1 {
		t.Errorf("expected 1 cleanup after release of expired entry, got %d", got)
	}
	if got := atomic.LoadInt64(setupCount); got != 1 {
		t.Errorf("setupCount tracking: got %d, want 1", got)
	}
}

// TestPooledWithEICTunnel_PanicPoisonsEntry pins that a panic
// from fn poisons the pool entry on the way out — refcount goes
// back to zero (no leak) and the entry is marked unusable so the
// next acquire builds fresh. Without the defer-release pattern, a
// panic would leave refcount=1 forever and the entry would never
// evict.
func TestPooledWithEICTunnel_PanicPoisonsEntry(t *testing.T) {
	setupCount, _, _, _ := withPoolSetupStub(t)
	poolTTL = 50 * time.Second
	globalEICTunnelPool = newEICTunnelPool()
	t.Cleanup(globalEICTunnelPool.stop)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("expected panic to bubble up, got nil")
			}
		}()
		_ = pooledWithEICTunnel(context.Background(), "i-panic",
			func(s eicSSHSession) error { panic("boom") })
	}()

	// Replenish the gate so the next setup can run.
	prev := poolSetupTunnel
	poolSetupTunnel = func(ctx context.Context, instanceID string) (
		eicSSHSession, func(), error) {
		atomic.AddInt64(setupCount, 1)
		return eicSSHSession{instanceID: instanceID}, func() {}, nil
	}
	t.Cleanup(func() { poolSetupTunnel = prev })

	// Next acquire must build fresh — entry was poisoned by panic.
	if err := pooledWithEICTunnel(context.Background(), "i-panic",
		func(s eicSSHSession) error { return nil }); err != nil {
		t.Fatalf("post-panic acquire: %v", err)
	}
	if got := atomic.LoadInt64(setupCount); got != 2 {
		t.Errorf("expected 2 setups (panic poisoned, rebuild), got %d", got)
	}
}

// TestPooledWithEICTunnel_PreservesFnErr pins that errors from the
// inner fn pass through to the caller verbatim — pool wrapping
// should not swallow or transform error semantics for app code.
func TestPooledWithEICTunnel_PreservesFnErr(t *testing.T) {
	withPoolSetupStub(t)
	poolTTL = 50 * time.Second

	// Reset the global pool so this test is isolated from any prior
	// test that may have populated it.
	globalEICTunnelPool = newEICTunnelPool()

	want := errors.New("file does not exist")
	got := pooledWithEICTunnel(context.Background(), "i-fn-err",
		func(s eicSSHSession) error { return want })
	if !errors.Is(got, want) {
		t.Errorf("pooledWithEICTunnel returned %v, want %v", got, want)
	}
}
