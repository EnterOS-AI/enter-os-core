package pendinguploads

import (
	"context"
	"time"
)

// StartSweeperWithIntervalForTest exposes startSweeperWithInterval to
// the external test package. The production code uses StartSeper
// (which pins the canonical SweepInterval); tests pin a short interval
// to exercise the ticker-driven cycle without burning real wall-clock
// time. The Go convention `export_test.go` keeps this seam OUT of the
// production binary — files ending in _test.go are stripped at build
// time, so this re-export only exists during `go test`.
func StartSweeperWithIntervalForTest(ctx context.Context, storage Storage, ackRetention, interval time.Duration) {
	startSweeperWithInterval(ctx, storage, ackRetention, interval, nil)
}

// StartSweeperForTest starts the sweeper and returns a done channel
// that is closed exactly once when the loop exits. Tests MUST receive
// from done before returning so the goroutine has fully terminated and
// the shared metric counters are stable for the next test's baseline
// capture (issue #86).
func StartSweeperForTest(ctx context.Context, storage Storage, ackRetention time.Duration) chan struct{} {
	done := make(chan struct{})
	go startSweeperWithInterval(ctx, storage, ackRetention, SweepInterval, done)
	return done
}
