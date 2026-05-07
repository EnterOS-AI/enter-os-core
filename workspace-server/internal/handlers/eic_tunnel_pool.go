package handlers

// eic_tunnel_pool.go — refcounted pool for EIC SSH tunnels keyed on
// instanceID. Reuses one tunnel across N file ops, amortising the
// ssh-keygen + SendSSHPublicKey + open-tunnel + waitForPort cost
// (~3-5s) over multiple cats/finds (~50-200ms each).
//
// Origin: core#11 — canvas detail-panel config + filesystem load
// took ~20s. ConfigTab fans out 4 GETs serially; the slowest is
// /files/config.yaml which dispatches to readFileViaEIC. Without a
// pool, every readFileViaEIC + listFilesViaEIC + writeFileViaEIC +
// deleteFileViaEIC pays the full setup cost even when fired
// back-to-back on the same workspace EC2.
//
// The pool keeps one eicSSHSession alive per instanceID for up to
// poolTTL. SendSSHPublicKey grants a 60s key validity, so poolTTL
// must stay strictly below that to avoid serving requests on a
// just-expired key. We default to 50s with a 10s safety margin.
//
// Concurrency model:
//
//   - Single mutex guards the entries map.
//   - Slow path (tunnel setup) runs OUTSIDE the lock, gated by an
//     "intent" placeholder so concurrent acquires for the same
//     instanceID don't both build a tunnel — the loser drops its
//     setup and uses the winner's.
//   - Refcount on each entry; eviction blocked while refcount > 0.
//   - Janitor goroutine sweeps every poolJanitorInterval, drops
//     entries where refcount == 0 && expiresAt < now.
//
// Test injection:
//
//   - poolSetupTunnel is a package-level var so tests can swap the
//     slow path for a counting stub. Production wires it to
//     realWithEICTunnel-style setup.
//   - withEICTunnel (the public, single-shot API) is also a var
//     (already, see template_files_eic.go). It's rebound here to
//     pooledWithEICTunnel which routes through globalEICTunnelPool.
//   - Tests that need single-shot behaviour can set poolTTL = 0,
//     which makes pooledWithEICTunnel fall through to the underlying
//     setup directly (no pool entry kept).

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// poolTTL is the maximum age of a pooled tunnel. Must be strictly
// less than the SendSSHPublicKey grant window (60s) so we never
// serve a request through a key that's about to expire mid-op.
//
// Configurable via init-time wiring (see initEICTunnelPool); not a
// const so tests can pin TTL=0 (disable pooling) or TTL=50ms (drive
// eviction tests).
var poolTTL = 50 * time.Second

// poolJanitorInterval is how often the janitor goroutine sweeps for
// expired idle entries. Tighter than poolTTL so eviction is timely;
// loose enough that the goroutine doesn't burn CPU.
var poolJanitorInterval = 10 * time.Second

// poolMaxEntries caps simultaneous instanceIDs the pool tracks.
// Beyond this, new acquires evict the LRU entry. Defends against a
// pathological caller (e.g. a sweep over hundreds of workspace
// EC2s) from leaking unbounded tunnel processes. 32 is a generous
// ceiling for the canvas use case (one human navigates ≤ ~5
// workspaces at a time).
var poolMaxEntries = 32

// poolSetupTunnel is the slow-path tunnel constructor. Wrapped in a
// var so tests can inject a counter stub. Returns a session and a
// cleanup function (closes the open-tunnel subprocess + scrubs the
// ephemeral keydir). nil session + non-nil err means setup failed
// and there is nothing to clean up.
//
// Production wiring lives in eic_tunnel_pool_setup.go (a thin shim
// over the existing realWithEICTunnel logic).
var poolSetupTunnel = func(ctx context.Context, instanceID string) (
	sess eicSSHSession, cleanup func(), err error) {
	return setupRealEICTunnel(ctx, instanceID)
}

// pooledTunnel is one entry in the pool. session is shared by N
// concurrent fn calls; cleanup runs once when refcount returns to
// zero AND the entry is past expiresAt or evicted.
//
// lastUsed tracks the most recent acquire time for LRU bookkeeping
// (overflow eviction). expiresAt is set at construction and not
// extended on use — a tunnel cannot live past poolTTL even if it's
// hot, because the underlying SendSSHPublicKey grant expires.
type pooledTunnel struct {
	session   eicSSHSession
	cleanup   func()
	expiresAt time.Time
	lastUsed  time.Time
	refcount  int
	poisoned  bool // true if a fn returned a tunnel-fatal error; do not reuse
}

// eicTunnelPool is the package-level pool. Single instance lives
// in globalEICTunnelPool; constructor runs lazily on first acquire.
type eicTunnelPool struct {
	mu      sync.Mutex
	entries map[string]*pooledTunnel
	// pendingSetups guards concurrent setup for the same instanceID.
	// First acquirer takes the slot; later ones wait on the channel.
	pendingSetups map[string]chan struct{}
	stopJanitor   chan struct{}
}

var (
	globalEICTunnelPool     *eicTunnelPool
	globalEICTunnelPoolOnce sync.Once
)

// getEICTunnelPool returns the singleton pool, lazy-initialising on
// first call. Idempotent.
func getEICTunnelPool() *eicTunnelPool {
	globalEICTunnelPoolOnce.Do(func() {
		globalEICTunnelPool = newEICTunnelPool()
		go globalEICTunnelPool.janitor()
	})
	return globalEICTunnelPool
}

// newEICTunnelPool constructs an empty pool. Exported so tests can
// build isolated pools without sharing the singleton.
func newEICTunnelPool() *eicTunnelPool {
	return &eicTunnelPool{
		entries:       map[string]*pooledTunnel{},
		pendingSetups: map[string]chan struct{}{},
		stopJanitor:   make(chan struct{}),
	}
}

// acquire returns a usable session for instanceID. If a healthy entry
// exists, refcount++ and return it. If a setup is in flight for the
// same instanceID, wait for it. Otherwise build one (slow path).
//
// done() must be called by the caller when the op finishes. It
// decrements refcount and triggers cleanup if the entry is past
// TTL or poisoned and refcount==0.
//
// Errors from the slow path propagate; pool state is not modified
// for failed setups (no poisoned entry created — that's only for
// fn-returned errors on a previously-good session).
func (p *eicTunnelPool) acquire(ctx context.Context, instanceID string) (
	sess eicSSHSession, done func(poisoned bool), err error) {

	if poolTTL <= 0 {
		// Pool disabled (TTL=0 mode for tests / opt-out). Fall
		// through to a direct setup with caller-driven cleanup.
		s, cleanup, err := poolSetupTunnel(ctx, instanceID)
		if err != nil {
			return eicSSHSession{}, nil, err
		}
		return s, func(_ bool) { cleanup() }, nil
	}

	for {
		p.mu.Lock()
		if pt, ok := p.entries[instanceID]; ok && !pt.poisoned && pt.expiresAt.After(time.Now()) {
			pt.refcount++
			pt.lastUsed = time.Now()
			p.mu.Unlock()
			return pt.session, p.releaser(instanceID, pt), nil
		}
		// Either no entry, expired entry, or poisoned entry. If a
		// setup is already in flight, wait and retry.
		if pending, ok := p.pendingSetups[instanceID]; ok {
			p.mu.Unlock()
			select {
			case <-pending:
				continue // re-check the entries map
			case <-ctx.Done():
				return eicSSHSession{}, nil, ctx.Err()
			}
		}
		// Drop expired/poisoned entry now (we'll cleanup outside
		// the lock — the entry is unreferenced or we'd not be here).
		var oldCleanup func()
		if pt, ok := p.entries[instanceID]; ok {
			if pt.refcount == 0 {
				oldCleanup = pt.cleanup
				delete(p.entries, instanceID)
			}
		}
		// Reserve the setup slot.
		signal := make(chan struct{})
		p.pendingSetups[instanceID] = signal
		p.mu.Unlock()

		if oldCleanup != nil {
			go oldCleanup()
		}

		// Slow path: build a new tunnel. Anything that goes wrong
		// here cleans up the pendingSetups slot and propagates to
		// the caller without leaving the pool in a state where the
		// next acquire blocks waiting on a signal that never fires.
		newSess, cleanup, setupErr := poolSetupTunnel(ctx, instanceID)

		p.mu.Lock()
		delete(p.pendingSetups, instanceID)
		close(signal)

		if setupErr != nil {
			p.mu.Unlock()
			return eicSSHSession{}, nil, fmt.Errorf("eic tunnel setup: %w", setupErr)
		}

		// Enforce LRU bound BEFORE inserting so we don't briefly
		// exceed the cap even by one entry.
		p.evictLRUIfFullLocked(instanceID)

		pt := &pooledTunnel{
			session:   newSess,
			cleanup:   cleanup,
			expiresAt: time.Now().Add(poolTTL),
			lastUsed:  time.Now(),
			refcount:  1,
		}
		p.entries[instanceID] = pt
		p.mu.Unlock()
		return pt.session, p.releaser(instanceID, pt), nil
	}
}

// releaser returns a closure that decrements refcount and triggers
// cleanup if (a) the entry is past TTL or (b) the caller signalled
// poison. Idempotent against double-release (decrements once via the
// captured pt; pool entry may have been replaced by then).
func (p *eicTunnelPool) releaser(instanceID string, pt *pooledTunnel) func(poisoned bool) {
	released := false
	return func(poisoned bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if released {
			return
		}
		released = true
		pt.refcount--
		if poisoned {
			pt.poisoned = true
		}
		// Evict immediately if poisoned-and-idle OR expired-and-idle.
		// Hot entries (refcount > 0) defer eviction to the last release.
		if pt.refcount == 0 && (pt.poisoned || pt.expiresAt.Before(time.Now())) {
			// If the entry in the map is still us, remove it.
			if cur, ok := p.entries[instanceID]; ok && cur == pt {
				delete(p.entries, instanceID)
			}
			go pt.cleanup()
		}
	}
}

// evictLRUIfFullLocked drops the least-recently-used IDLE entry
// when the pool is at capacity. Caller must hold p.mu. The new
// instanceID about to be inserted is excluded so we don't evict
// ourselves. If no idle entries exist, no eviction happens — the
// new entry will push us above the soft cap until something releases.
func (p *eicTunnelPool) evictLRUIfFullLocked(skipInstance string) {
	if len(p.entries) < poolMaxEntries {
		return
	}
	var oldestKey string
	var oldest *pooledTunnel
	for k, pt := range p.entries {
		if k == skipInstance {
			continue
		}
		if pt.refcount > 0 {
			continue
		}
		if oldest == nil || pt.lastUsed.Before(oldest.lastUsed) {
			oldestKey = k
			oldest = pt
		}
	}
	if oldest == nil {
		return // every entry is in use; no eviction possible
	}
	delete(p.entries, oldestKey)
	go oldest.cleanup()
}

// janitor periodically scans for entries that are idle AND expired,
// closing their tunnels. Runs forever (per pool lifetime); cancelled
// by close(p.stopJanitor) for tests that build short-lived pools.
func (p *eicTunnelPool) janitor() {
	t := time.NewTicker(poolJanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.sweep()
		case <-p.stopJanitor:
			return
		}
	}
}

// sweep is one janitor pass. Drops idle expired entries.
func (p *eicTunnelPool) sweep() {
	p.mu.Lock()
	now := time.Now()
	var toClose []func()
	for k, pt := range p.entries {
		if pt.refcount == 0 && pt.expiresAt.Before(now) {
			toClose = append(toClose, pt.cleanup)
			delete(p.entries, k)
		}
	}
	p.mu.Unlock()
	for _, c := range toClose {
		go c()
	}
}

// stop terminates the janitor and closes all idle entries. Hot
// (refcount > 0) entries are NOT force-closed — callers running
// against them would see a use-after-free. In practice stop is only
// called by tests that have already drained their callers.
func (p *eicTunnelPool) stop() {
	close(p.stopJanitor)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pt := range p.entries {
		if pt.refcount == 0 {
			go pt.cleanup()
			delete(p.entries, k)
		}
	}
}

// pooledWithEICTunnel is the pool-backed replacement for
// realWithEICTunnel. The signature matches `var withEICTunnel`
// exactly so the rebind (in initEICTunnelPool) is a drop-in.
//
// Errors from `fn` itself are forwarded to the caller AND mark the
// pool entry as poisoned, so the next acquire builds a fresh
// tunnel. This catches the case where the workspace EC2 was
// restarted out-of-band (tunnel still appears alive locally but
// every cat/find errors out).
func pooledWithEICTunnel(ctx context.Context, instanceID string,
	fn func(s eicSSHSession) error) error {
	pool := getEICTunnelPool()
	sess, done, err := pool.acquire(ctx, instanceID)
	if err != nil {
		return err
	}
	// poisoned defaults to true so a panic from fn poisons the
	// entry on the way through the deferred release. Without the
	// defer, a panicking fn would leak refcount=1 forever and
	// permanently block eviction of this entry. The fn-error path
	// resets poisoned to its real classification before return.
	poisoned := true
	defer func() { done(poisoned) }()
	fnErr := fn(sess)
	poisoned = fnErrIndicatesTunnelFault(fnErr)
	return fnErr
}

// fnErrIndicatesTunnelFault returns true for fn errors whose nature
// suggests the underlying tunnel is no longer reusable (auth gone,
// network gone, ssh process dead). Returning true poisons the pool
// entry so the next acquire builds fresh.
//
// Conservative: only marks tunnel-faulty for clearly tunnel-level
// failures (connection refused, broken pipe, ssh exit-status from
// fatal-channel signals). A `cat` returning os.ErrNotExist on a
// missing file is NOT a tunnel fault — that's the file path being
// wrong, the tunnel is fine.
func fnErrIndicatesTunnelFault(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// stderr substrings produced by ssh when the tunnel is broken.
	for _, marker := range []string{
		"connection refused",
		"connection closed",
		"broken pipe",
		"Connection reset by peer",
		"kex_exchange_identification",
		"port forwarding failed",
		"Permission denied",
		"Authentication failed",
	} {
		if containsCaseInsensitive(msg, marker) {
			return true
		}
	}
	return false
}

// containsCaseInsensitive avoids importing strings just for this
// (the file already needs ssh stderr matching elsewhere — this
// keeps the helper local to avoid a cross-file dependency).
func containsCaseInsensitive(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	// Manual lowercase compare loop; ssh error markers are ASCII so
	// no need for unicode-aware folding.
	low := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if low(s[i+j]) != low(substr[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// initEICTunnelPool rebinds the package-level withEICTunnel var to
// the pooled implementation. Called once at package init via the
// init() in eic_tunnel_pool_setup.go (split file so the rebind
// itself is testable without dragging in the production setup
// shim's exec/aws dependencies).
func initEICTunnelPool() {
	withEICTunnel = pooledWithEICTunnel
}
