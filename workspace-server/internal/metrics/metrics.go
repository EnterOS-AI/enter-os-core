// Package metrics provides a lightweight Prometheus-format metrics endpoint
// for the Molecule AI platform. It requires no external dependencies — all
// serialization is done against the Prometheus text exposition format (v0.0.4)
// using the Go standard library.
//
// Exposed metrics:
//
//	molecule_http_requests_total{method,path,status}      - counter
//	molecule_http_request_duration_seconds{method,path}   - counter (sum, for avg rate)
//	molecule_websocket_connections_active                  - gauge
//	molecule_pending_uploads_swept_total{outcome}          - counter (acked|expired|error)
//	go_goroutines                                          - gauge
//	go_memstats_alloc_bytes                                - gauge
//	go_memstats_sys_bytes                                  - gauge
//	go_memstats_heap_inuse_bytes                           - gauge
//	go_gc_duration_seconds_total                           - counter
package metrics

import (
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// reqKey indexes per-route request counts and latency sums.
type reqKey struct {
	method string
	path   string
	status int
}

var (
	mu            sync.RWMutex
	reqCounts     = map[reqKey]int64{}   // molecule_http_requests_total
	reqDurSums    = map[reqKey]float64{} // sum of durations (seconds)
	activeWSConns int64                  // molecule_websocket_connections_active

	// pendinguploads sweeper counters — atomic so the sweeper goroutine
	// doesn't contend with the /metrics handler.
	pendingUploadsSweptAcked   int64 // molecule_pending_uploads_swept_total{outcome="acked"}
	pendingUploadsSweptExpired int64 // molecule_pending_uploads_swept_total{outcome="expired"}
	pendingUploadsSweepErrors  int64 // molecule_pending_uploads_swept_total{outcome="error"}
)

// Middleware records per-request counts and latency.
// Register this before route handlers in the Gin engine.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		duration := time.Since(start).Seconds()
		// Use the matched route pattern (e.g. "/workspaces/:id") so high-cardinality
		// workspace UUIDs don't explode the label space.
		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}

		k := reqKey{
			method: c.Request.Method,
			path:   path,
			status: c.Writer.Status(),
		}

		mu.Lock()
		reqCounts[k]++
		reqDurSums[k] += duration
		mu.Unlock()
	}
}

// TrackWSConnect increments the active WebSocket connections gauge.
// Call from the WebSocket upgrade handler after a successful upgrade.
func TrackWSConnect() { atomic.AddInt64(&activeWSConns, 1) }

// TrackWSDisconnect decrements the active WebSocket connections gauge.
// Call from the WebSocket disconnect / cleanup path.
func TrackWSDisconnect() { atomic.AddInt64(&activeWSConns, -1) }

// phantomBusyResets is the cumulative count of workspace rows the
// phantom-busy sweep reset (active_tasks=0 → active_tasks=0+counter
// cleared). Surfaced as molecule_phantom_busy_resets_total — a high
// reset rate signals a regression in task-lifecycle accounting (most
// often: missing env vars cause claude --print to time out, the
// agent loop never decrements active_tasks, and the sweep cleans up
// the counter ~10 min later). Issue #2865.
var phantomBusyResets int64

// TrackPhantomBusyReset increments the phantom-busy reset counter.
// Called from sweepPhantomBusy in workspace-server/internal/scheduler/
// after each row whose active_tasks was reset to 0. Idempotent +
// goroutine-safe; called once per row per sweep tick.
func TrackPhantomBusyReset() { atomic.AddInt64(&phantomBusyResets, 1) }

// PendingUploadsSwept records a successful sweep cycle. acked/expired
// are added to the per-outcome counters so dashboards can spot the
// stuck-fetch pattern (high expired, low acked) vs healthy churn.
func PendingUploadsSwept(acked, expired int) {
	if acked > 0 {
		atomic.AddInt64(&pendingUploadsSweptAcked, int64(acked))
	}
	if expired > 0 {
		atomic.AddInt64(&pendingUploadsSweptExpired, int64(expired))
	}
}

// PendingUploadsSweepError records a sweeper-cycle failure (transient
// DB error etc). Counted separately so the rate of errored sweeps is
// observable independent of how many rows the successful sweeps deleted.
func PendingUploadsSweepError() {
	atomic.AddInt64(&pendingUploadsSweepErrors, 1)
}

// PendingUploadsSweepCounts returns the current (acked, expired, error)
// totals. Exposed for tests that need a deterministic delta probe of
// the sweeper's metric writes — the /metrics endpoint is the production
// observability surface; this is a unit-test escape hatch.
func PendingUploadsSweepCounts() (acked, expired, errored int64) {
	return atomic.LoadInt64(&pendingUploadsSweptAcked),
		atomic.LoadInt64(&pendingUploadsSweptExpired),
		atomic.LoadInt64(&pendingUploadsSweepErrors)
}

// Handler returns a Gin handler that serialises all collected metrics in
// Prometheus text exposition format (v0.0.4). Mount this at GET /metrics.
func Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w := c.Writer
		w.WriteHeader(http.StatusOK)

		// ── Go runtime ─────────────────────────────────────────────────────
		writeln(w, "# HELP go_goroutines Number of goroutines currently running.")
		writeln(w, "# TYPE go_goroutines gauge")
		fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())

		writeln(w, "# HELP go_memstats_alloc_bytes Bytes of allocated heap objects.")
		writeln(w, "# TYPE go_memstats_alloc_bytes gauge")
		fmt.Fprintf(w, "go_memstats_alloc_bytes %d\n", ms.Alloc)

		writeln(w, "# HELP go_memstats_sys_bytes Total bytes of memory obtained from the OS.")
		writeln(w, "# TYPE go_memstats_sys_bytes gauge")
		fmt.Fprintf(w, "go_memstats_sys_bytes %d\n", ms.Sys)

		writeln(w, "# HELP go_memstats_heap_inuse_bytes Bytes in in-use heap spans.")
		writeln(w, "# TYPE go_memstats_heap_inuse_bytes gauge")
		fmt.Fprintf(w, "go_memstats_heap_inuse_bytes %d\n", ms.HeapInuse)

		writeln(w, "# HELP go_gc_duration_seconds_total Cumulative GC pause time.")
		writeln(w, "# TYPE go_gc_duration_seconds_total counter")
		fmt.Fprintf(w, "go_gc_duration_seconds_total %g\n", float64(ms.PauseTotalNs)/1e9)

		// ── Molecule AI HTTP ───────────────────────────────────────────────────
		writeln(w, "# HELP molecule_http_requests_total Total HTTP requests served, by method, path, and status.")
		writeln(w, "# TYPE molecule_http_requests_total counter")

		writeln(w, "# HELP molecule_http_request_duration_seconds_total Cumulative HTTP request duration in seconds.")
		writeln(w, "# TYPE molecule_http_request_duration_seconds_total counter")

		// Snapshot under lock, then write unlocked (avoids holding lock during slow HTTP writes)
		mu.RLock()
		countsCopy := make(map[reqKey]int64, len(reqCounts))
		for k, v := range reqCounts {
			countsCopy[k] = v
		}
		durCopy := make(map[reqKey]float64, len(reqDurSums))
		for k, v := range reqDurSums {
			durCopy[k] = v
		}
		mu.RUnlock()

		for k, count := range countsCopy {
			fmt.Fprintf(w,
				"molecule_http_requests_total{method=%q,path=%q,status=\"%d\"} %d\n",
				k.method, k.path, k.status, count,
			)
		}
		for k, sum := range durCopy {
			fmt.Fprintf(w,
				"molecule_http_request_duration_seconds_total{method=%q,path=%q,status=\"%d\"} %g\n",
				k.method, k.path, k.status, sum,
			)
		}

		// ── Molecule AI WebSocket ──────────────────────────────────────────────
		writeln(w, "# HELP molecule_websocket_connections_active Number of active WebSocket connections.")
		writeln(w, "# TYPE molecule_websocket_connections_active gauge")
		fmt.Fprintf(w, "molecule_websocket_connections_active %d\n", atomic.LoadInt64(&activeWSConns))

		// ── Molecule AI scheduler ──────────────────────────────────────────────
		writeln(w, "# HELP molecule_phantom_busy_resets_total Cumulative count of workspace rows reset by the phantom-busy sweep (active_tasks cleared after >10 min of activity_log silence). High reset rate signals task-lifecycle accounting regressions — see issue #2865.")
		writeln(w, "# TYPE molecule_phantom_busy_resets_total counter")
		fmt.Fprintf(w, "molecule_phantom_busy_resets_total %d\n", atomic.LoadInt64(&phantomBusyResets))

		// ── Pending-uploads sweeper ────────────────────────────────────────────
		writeln(w, "# HELP molecule_pending_uploads_swept_total Pending-uploads rows deleted by the GC sweeper, by outcome.")
		writeln(w, "# TYPE molecule_pending_uploads_swept_total counter")
		fmt.Fprintf(w, "molecule_pending_uploads_swept_total{outcome=\"acked\"} %d\n",
			atomic.LoadInt64(&pendingUploadsSweptAcked))
		fmt.Fprintf(w, "molecule_pending_uploads_swept_total{outcome=\"expired\"} %d\n",
			atomic.LoadInt64(&pendingUploadsSweptExpired))
		fmt.Fprintf(w, "molecule_pending_uploads_swept_total{outcome=\"error\"} %d\n",
			atomic.LoadInt64(&pendingUploadsSweepErrors))
	}
}

func writeln(w http.ResponseWriter, s string) {
	fmt.Fprintln(w, s)
}
