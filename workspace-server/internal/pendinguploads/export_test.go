package pendinguploads

import (
	"context"
	"time"
)

// StartSweeperWithIntervalForTest exposes startSweeperWithInterval to
// the external test package. The production code uses StartSweeper
// (which pins the canonical SweepInterval); tests pin a short interval
// to exercise the ticker-driven cycle without burning real wall-clock
// time. The Go convention `export_test.go` keeps this seam OUT of the
// production binary — files ending in _test.go are stripped at build
// time, so this re-export only exists during `go test`.
func StartSweeperWithIntervalForTest(ctx context.Context, storage Storage, ackRetention, interval time.Duration) {
	startSweeperWithInterval(ctx, storage, ackRetention, interval)
}
