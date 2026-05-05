// sweeper.go — periodic GC for the pending_uploads table.
//
// The platform's poll-mode chat-upload handler creates a row in
// pending_uploads for every chat-attached file the canvas sends to a
// poll-mode workspace. The workspace's inbox poller fetches the bytes
// and acks the row, but two failure modes leak rows long-term:
//
//  1. Workspace fetches but never acks (network hiccup between GET
//     /content and POST /ack; workspace crashed between the two).
//     Phase 1's Get refuses to re-serve an acked row, but a never-
//     acked row could in principle be fetched repeatedly until expires_at.
//     Phase 2's workspace-side fetcher is idempotent; the worry is
//     only disk usage on the platform side.
//
//  2. Workspace never fetches at all (workspace was offline when the
//     row was written; the upload's TTL elapsed).
//
// This sweeper handles both. It runs every SweepInterval, deletes rows
// in either category, and emits structured logs + Prometheus counters
// so a stuck-fetch dashboard can spot the leak class.
//
// Failure isolation: a transient DB error must NOT crash the sweeper.
// We log + continue; the next tick retries. ctx cancellation cleanly
// shuts the loop down for graceful shutdown.

package pendinguploads

import (
	"context"
	"log"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/metrics"
)

// SweepInterval is the cadence of the GC loop. 5 minutes is a balance
// between "rows reaped quickly enough that disk usage doesn't surprise
// anyone" and "we don't pay a DELETE round-trip every 30 seconds when
// there are no candidates." Aligned with other low-priority sweepers
// (registry/orphan_sweeper runs at 60s but operates on Docker — much
// more expensive per cycle than a single indexed DELETE).
const SweepInterval = 5 * time.Minute

// DefaultAckRetention is how long an acked row sticks around before the
// sweeper deletes it. 1 hour gives the workspace enough time to retry
// the GET if its first fetch crashed mid-write — at-least-once handoff
// without leaking content for a full 24h after the workspace already
// has a copy.
const DefaultAckRetention = 1 * time.Hour

// sweepDeadline bounds a single sweep cycle. A daemon at the edge of
// timeout shouldn't pile up goroutines; 30s is generous for a single
// indexed DELETE on a table that should rarely have more than a few
// thousand rows in flight.
const sweepDeadline = 30 * time.Second

// StartSweeper runs the GC loop until ctx is cancelled. nil storage
// makes the loop a no-op (matches the handlers' tolerance for an
// unconfigured pendinguploads — some test harnesses run without the
// storage wired).
//
// Pass ackRetention=0 to use DefaultAckRetention. Negative values are
// clamped at the storage layer.
//
// Production callers use SweepInterval (5m). Tests use a short interval
// to exercise the ticker-driven sweep path without burning real wall-
// clock time.
func StartSweeper(ctx context.Context, storage Storage, ackRetention time.Duration) {
	startSweeperWithInterval(ctx, storage, ackRetention, SweepInterval)
}

// startSweeperWithInterval is the test-friendly variant of StartSweeper
// — same loop, but the cadence is caller-specified. Production code
// should use StartSweeper to keep the SweepInterval constant pinned.
func startSweeperWithInterval(ctx context.Context, storage Storage, ackRetention, interval time.Duration) {
	if storage == nil {
		log.Println("pendinguploads sweeper: storage is nil — sweeper disabled")
		return
	}
	if ackRetention == 0 {
		ackRetention = DefaultAckRetention
	}
	log.Printf(
		"pendinguploads sweeper started — sweeping every %s; ack retention %s",
		interval, ackRetention,
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Run once immediately so a platform restart cleans up any rows
	// that became eligible while we were down — don't make the
	// operator wait 5 minutes for the first sweep.
	sweepOnce(ctx, storage, ackRetention)
	for {
		select {
		case <-ctx.Done():
			log.Println("pendinguploads sweeper: shutdown")
			return
		case <-ticker.C:
			sweepOnce(ctx, storage, ackRetention)
		}
	}
}

func sweepOnce(parent context.Context, storage Storage, ackRetention time.Duration) {
	ctx, cancel := context.WithTimeout(parent, sweepDeadline)
	defer cancel()

	res, err := storage.Sweep(ctx, ackRetention)
	if err != nil {
		// Transient errors: log + continue. The next tick retries; if
		// the DB is genuinely down, the rest of the platform is also
		// broken and disk usage is the least of the operator's
		// problems.
		log.Printf("pendinguploads sweeper: Sweep failed: %v", err)
		metrics.PendingUploadsSweepError()
		return
	}
	metrics.PendingUploadsSwept(res.Acked, res.Expired)
	if res.Total() > 0 {
		// Per-cycle structured-ish log (one line per cycle that did
		// something). Quiet by design — most cycles delete zero rows
		// on a healthy system, and a stream of empty-result lines
		// would drown the production log without surfacing a signal.
		log.Printf(
			"pendinguploads sweeper: deleted acked=%d expired=%d total=%d",
			res.Acked, res.Expired, res.Total(),
		)
	}
}
