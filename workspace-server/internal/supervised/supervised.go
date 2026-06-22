// Package supervised provides a panic-recovering supervisor for long-running
// background goroutines on the platform. Every "go X.Start(ctx)" invocation
// in main.go should go through [RunWithRecover] so a single panic from one
// tenant's data cannot silently kill a subsystem that serves every tenant.
//
// Incident that motivated this (issue #85, 2026-04-14):
//
//   The cron scheduler goroutine died silently at 14:21 UTC and stayed dead
//   for 12+ hours. Platform restart didn't recover it. Root cause: no
//   defer recover() in the tick loop. Observable signals (HTTP 200, container
//   healthy, DB healthy) all stayed green — only the subsystem was dead.
//
// In a multi-tenant SaaS deployment the blast radius is every tenant
// simultaneously, which is exactly the class of failure we cannot afford.
package supervised

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// Default backoff bounds for RunWithRecover restarts.
const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// RunWithRecover runs fn in a recover wrapper. If fn panics, the panic is
// logged with its stack trace and fn is restarted after an exponential
// backoff (capped at maxBackoff). The loop exits cleanly when ctx is done.
//
// fn is expected to be a long-running loop (e.g. "for { select { ticker ... } }").
// If fn returns without panicking (e.g. ctx.Done), RunWithRecover returns.
//
//	go supervised.RunWithRecover(ctx, "scheduler", func(c context.Context) {
//	    scheduler.Start(c)
//	})
//
// name is used in log lines and by the liveness registry below.
func RunWithRecover(ctx context.Context, name string, fn func(context.Context)) {
	backoff := initialBackoff
	for {
		select {
		case <-ctx.Done():
			log.Printf("supervised[%s]: context done; stopping", name)
			return
		default:
		}

		panicked := runOnce(ctx, name, fn)

		// Clean return → the goroutine decided to stop (likely ctx.Done inside fn).
		// Don't restart.
		if !panicked {
			log.Printf("supervised[%s]: returned cleanly; not restarting", name)
			return
		}

		// Panic → back off and restart.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// runOnce invokes fn with recover. Returns true iff fn panicked.
func runOnce(ctx context.Context, name string, fn func(context.Context)) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			log.Printf("supervised[%s]: PANIC recovered: %v\n%s", name, r, debug.Stack())
		}
	}()
	fn(ctx)
	return false
}

// --- Liveness registry -----------------------------------------------------
//
// Each subsystem calls Heartbeat(name) at the end of each tick / iteration.
// Operators read the registry via /admin/liveness to detect stuck-but-not-
// crashed subsystems (e.g. a tick that deadlocks without panicking).

var (
	livenessMu sync.RWMutex
	lastTicks  = map[string]time.Time{}
)

// Heartbeat records that subsystem `name` is alive as of now.
func Heartbeat(name string) {
	livenessMu.Lock()
	lastTicks[name] = time.Now()
	livenessMu.Unlock()
}

// LastTick returns the wall-clock time of the most recent Heartbeat for
// subsystem `name`. Returns the zero time if the subsystem has never
// heartbeated.
func LastTick(name string) time.Time {
	livenessMu.RLock()
	defer livenessMu.RUnlock()
	return lastTicks[name]
}

// Snapshot returns a copy of every subsystem's last-tick time, for admin
// endpoints.
func Snapshot() map[string]time.Time {
	livenessMu.RLock()
	defer livenessMu.RUnlock()
	out := make(map[string]time.Time, len(lastTicks))
	for k, v := range lastTicks {
		out[k] = v
	}
	return out
}

// IsHealthy returns true iff every subsystem in `expected` has tickled
// within `staleThreshold` ago. Use from /health (or a strict variant of it)
// to surface stuck subsystems to an external orchestrator.
func IsHealthy(expected []string, staleThreshold time.Duration) (healthy bool, stale []string) {
	livenessMu.RLock()
	defer livenessMu.RUnlock()
	now := time.Now()
	for _, name := range expected {
		last, ok := lastTicks[name]
		if !ok || now.Sub(last) > staleThreshold {
			stale = append(stale, name)
		}
	}
	return len(stale) == 0, stale
}
