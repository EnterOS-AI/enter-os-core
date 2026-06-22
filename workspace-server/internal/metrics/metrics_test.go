package metrics

// Tests for the phantom-busy reset counter wired up by issue #2865.
// The counter is exposed at /metrics as
// molecule_phantom_busy_resets_total. A high steady-state value
// signals task-lifecycle accounting regressions in the agent loop —
// see scheduler.sweepPhantomBusy for the writer.

import (
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// resetForTest zeroes the counter so a single test's TrackPhantomBusyReset
// calls don't compound onto a previous test's run. metrics.go's package-
// level state means every test that touches the counter must reset.
func resetForTest() {
	atomic.StoreInt64(&phantomBusyResets, 0)
}

func TestTrackPhantomBusyReset_IncrementsCounter(t *testing.T) {
	resetForTest()
	for i := 0; i < 7; i++ {
		TrackPhantomBusyReset()
	}
	got := atomic.LoadInt64(&phantomBusyResets)
	if got != 7 {
		t.Errorf("counter after 7 calls = %d, want 7", got)
	}
}

func TestTrackPhantomBusyReset_RaceFreeUnderConcurrentWrites(t *testing.T) {
	resetForTest()
	var wg sync.WaitGroup
	const goroutines = 50
	const callsPerGoroutine = 200
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				TrackPhantomBusyReset()
			}
		}()
	}
	wg.Wait()
	want := int64(goroutines * callsPerGoroutine)
	got := atomic.LoadInt64(&phantomBusyResets)
	if got != want {
		t.Errorf("counter under concurrent writes = %d, want %d (lost increments → atomic broken)",
			got, want)
	}
}

func TestHandler_ExposesPhantomBusyResetsCounter(t *testing.T) {
	resetForTest()
	for i := 0; i < 3; i++ {
		TrackPhantomBusyReset()
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/metrics", Handler())

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	r.ServeHTTP(w, req)

	body := w.Body.String()
	// HELP + TYPE lines must precede the metric (Prometheus text exposition format).
	if !strings.Contains(body, "# HELP molecule_phantom_busy_resets_total") {
		t.Errorf("metrics output missing HELP line for molecule_phantom_busy_resets_total:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE molecule_phantom_busy_resets_total counter") {
		t.Errorf("metrics output missing TYPE line for molecule_phantom_busy_resets_total:\n%s", body)
	}
	if !strings.Contains(body, "molecule_phantom_busy_resets_total 3\n") {
		t.Errorf("metrics output missing counter value 3:\n%s", body)
	}
}

func TestHandler_PhantomBusyResetsZeroByDefault(t *testing.T) {
	// Fresh process should report 0 — pin the contract so a future
	// refactor that lazy-inits the counter to nil doesn't silently
	// drop the metric from /metrics.
	resetForTest()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/metrics", Handler())

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	r.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "molecule_phantom_busy_resets_total 0\n") {
		t.Errorf("metric must report 0 by default:\n%s", w.Body.String())
	}
}
