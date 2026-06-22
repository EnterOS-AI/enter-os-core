package middleware

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRecoverPanic_RecoversFromPanicInGoroutine: the core#2125
// family of fixes added `recover()` wrappers to 6 long-lived
// background goroutines (cache sweepers, rate-limit cleanup, session
// cache, A2A SSE idle watcher, terminal bridges, importer provision
// goroutine). The wrapper was refactored into a single
// `recoverPanic(prefix)` helper so the panic-recovery contract is
// testable in one place. This test proves the contract:
//
//   1. A goroutine that defers `recoverPanic(...)` and then panics
//      does NOT crash the test process — the panic is contained.
//   2. The recovered value is logged with the caller's prefix — so
//      operators can grep for the specific goroutine that tripped.
//   3. The goroutine's deferred epilogue (e.g. `close(done)`) STILL
//      runs after the panic — the wrapper returns normally, so any
//      `defer close(done)` placed BEFORE the recoverPanic defer
//      fires (defers run in LIFO; the close runs after the recover).
//
// Runtime: sub-millisecond.
func TestRecoverPanic_RecoversFromPanicInGoroutine(t *testing.T) {
	// Capture the log output so we can assert the prefix appears
	// and the recovered value is included. The buffer is wrapped
	// in a mutex (syncBuffer below) so the goroutine's log.Printf
	// write and the test's buf.String() read are synchronized —
	// core#2834: the prior `bytes.Buffer` direct-use was a
	// concurrent read/write that the -race detector flagged.
	var buf syncBuffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})

	done := make(chan struct{})
	go func() {
		// Defer order matters: `defer close(done)` is registered
		// FIRST, `defer recoverPanic(...)` is registered LAST.
		// LIFO execution runs recoverPanic first (which writes
		// the log line via log.Printf), then close(done) — so
		// the test's <-done unblock happens AFTER the log write
		// is complete. The prior code had the defers in the
		// opposite order, which let close(done) fire before
		// log.Printf finished writing to the buffer (core#2834
		// data race).
		defer close(done)
		defer recoverPanic("test_goroutine")
		panic("simulated panic value")
	}()

	// Bound the wait so a regressed recoverPanic (one that fails to
	// actually recover) surfaces as a test hang, not a CI timeout.
	select {
	case <-done:
		// Recovered and returned cleanly.
	case <-time.After(1 * time.Second):
		t.Fatal("goroutine did not recover within 1s — recoverPanic is broken (or the panic is propagating)")
	}

	// Assert the log line mentions the prefix AND the recovered value.
	out := buf.String()
	if !strings.Contains(out, "test_goroutine") {
		t.Errorf("recoverPanic log should include the prefix; got: %q", out)
	}
	if !strings.Contains(out, "simulated panic value") {
		t.Errorf("recoverPanic log should include the recovered panic value; got: %q", out)
	}
	if !strings.Contains(out, "PANIC") {
		t.Errorf("recoverPanic log should include the 'PANIC' marker for grep-ability; got: %q", out)
	}
}

// TestRecoverPanic_NoopOnNormalReturn: a goroutine that defers
// recoverPanic(...) and returns normally must NOT log anything —
// `recover()` returns nil when there was no panic, and the wrapper
// must not emit a spurious "PANIC" line in that case (which would
// page operators for nothing and obscure the real signal).
func TestRecoverPanic_NoopOnNormalReturn(t *testing.T) {
	// Use the same mutex-guarded buffer (core#2834) so this test
	// stays -race-clean if a future change ever makes recoverPanic
	// write to the buffer on the normal-return path.
	var buf syncBuffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})

	done := make(chan struct{})
	go func() {
		// Same defer order as the panic-recovery test: close(done)
		// registered first so LIFO runs recoverPanic first (no-op
		// on the normal-return path, but keeps the pattern uniform
		// and future-proof).
		defer close(done)
		defer recoverPanic("normal_return_goroutine")
		// No panic — just return.
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("normal-return goroutine did not finish in 1s")
	}

	if out := buf.String(); strings.Contains(out, "PANIC") {
		t.Errorf("recoverPanic must NOT log on a normal return; got: %q", out)
	}
}

// (REMOVED) TestHTTPPanicRecovery_HandlerPanicReturns500:
//
//   Originally added in commit e4b649c9 to address CR2's first
//   round of feedback ("exercise the real HTTP panic-recovery
//   path"). On the second round of review, CR2 correctly flagged
//   that the test wired a synthetic `gin.New() + gin.Recovery()`
//   engine rather than the production `router.Setup` flow (which
//   uses `gin.Default()`). Exercising the production router
//   requires constructing ~10 dependencies (Hub, Broadcaster,
//   Provisioner, handlers.WorkspaceHandler, etc.) — out of scope
//   for this regression-test PR.
//
//   Per CR2's option 2 in the second review, the synthetic
//   handler-panic coverage has been REMOVED from the PR. The
//   PR's merge-blocking coverage is now narrowed to:
//     (a) the real http client.Timeout regression (10s slow-
//         upstream test against the production refreshEnvFromCP)
//     (b) the recoverPanic helper contract (covered for the 3
//         in-production call sites in internal/middleware: the
//         session_auth sweeper, ratelimit cleanup, mcp_ratelimit
//         cleanup)
//   Adding a real production-router handler-panic regression is
//   a candidate for a follow-up PR — the work is "make router.Setup
//   testable with the minimum dependency surface, then assert
//   500+no-crash through the real path", which is a non-trivial
//   refactor on its own.

// syncBuffer wraps a bytes.Buffer with a mutex so the log goroutine
// (called via log.Printf -> log.Output -> Write) and the test's
// subsequent String() read are serialized. Without this wrapper,
// core#2834 flagged a concurrent read/write on the underlying
// bytes.Buffer that the -race detector caught intermittently on
// the repo-wide `go test -race` step.
//
// Why a mutex and not a channel: the standard `log` package
// already holds its OWN internal mutex around calls to
// io.Writer.Write, but that mutex does NOT synchronize with
// reads from the test goroutine — it only serializes concurrent
// writers. The test's buf.String() read from a separate goroutine
// is a separate access path that the log package knows nothing
// about, so the race detector flags the (write, read) pair.
// syncBuffer provides the cross-goroutine happens-before edge
// the race detector needs to see.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write satisfies io.Writer for log.SetOutput. Holds the mutex
// around the underlying bytes.Buffer.Write call.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String returns a snapshot of the buffer's contents. Holds the
// mutex around the underlying bytes.Buffer.String call so the
// read is synchronized with concurrent Write calls.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
