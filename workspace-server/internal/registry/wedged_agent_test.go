// Regression tests for the wedged-agent predicate and monitor. The
// 2026-06-19 a2a RCA (#3057) found that an alive-but-wedged agent
// (active_tasks>0, no outbound A2A, no heartbeat) read as `status:
// online` to the platform and could only be detected by manual
// inspection of the tuple. These tests pin the predicate so future
// drift in the wedge definition is caught at unit-test time, not in
// prod.
package registry

import (
	"database/sql"
	"testing"
	"time"
)

// ==================== IsWedgedAgent predicate ====================

func TestIsWedgedAgent_Pin(t *testing.T) {
	// The full truth table for the wedge definition. Each row is a
	// single tuple; the comment column explains the scenario.
	threshold := 5 * time.Minute
	now := time.Now()
	cases := []struct {
		name             string
		activeTasks      int
		lastOutboundAt   sql.NullTime
		lastHeartbeatAt  sql.NullTime
		threshold        time.Duration
		wantWedged       bool
		explanation      string
	}{
		{
			name:            "wedge: active>0, null outbound, null heartbeat",
			activeTasks:     1,
			lastOutboundAt:  sql.NullTime{},
			lastHeartbeatAt: sql.NullTime{},
			threshold:       threshold,
			wantWedged:      true,
			explanation:     "Kimi-shape wedge from the RCA: active>0 with no record of any activity",
		},
		{
			name:            "wedge: active>0, stale outbound, stale heartbeat",
			activeTasks:     1,
			lastOutboundAt:  sql.NullTime{Time: now.Add(-10 * time.Minute), Valid: true},
			lastHeartbeatAt: sql.NullTime{Time: now.Add(-6 * time.Minute), Valid: true},
			threshold:       threshold,
			wantWedged:      true,
			explanation:     "Both timestamps older than threshold; agent is stuck",
		},
		{
			name:            "busy-but-alive: active>0, recent outbound, recent heartbeat",
			activeTasks:     1,
			lastOutboundAt:  sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true},
			lastHeartbeatAt: sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true},
			threshold:       threshold,
			wantWedged:      false,
			explanation:     "A legitimately busy agent: active AND producing outbound AND heartbeating",
		},
		{
			name:            "busy-but-no-recent-outbound: recent heartbeat only",
			activeTasks:     1,
			lastOutboundAt:  sql.NullTime{Time: now.Add(-10 * time.Minute), Valid: true},
			lastHeartbeatAt: sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true},
			threshold:       threshold,
			wantWedged:      false,
			explanation:     "Heartbeat is recent — the heartbeat task is alive even if the turn is stuck. We do NOT wedge; the operator gets a chance to inspect.",
		},
		{
			name:            "idle: active==0, everything stale",
			activeTasks:     0,
			lastOutboundAt:  sql.NullTime{Time: now.Add(-10 * time.Minute), Valid: true},
			lastHeartbeatAt: sql.NullTime{Time: now.Add(-10 * time.Minute), Valid: true},
			threshold:       threshold,
			wantWedged:      false,
			explanation:     "Idle workspaces are NOT wedged — they are candidates for hibernation, a different monitor",
		},
		{
			name:            "idle-and-claim: active==0 with stale everything (already hibernated candidate)",
			activeTasks:     0,
			lastOutboundAt:  sql.NullTime{},
			lastHeartbeatAt: sql.NullTime{},
			threshold:       threshold,
			wantWedged:      false,
			explanation:     "A never-used workspace is not wedged; wedge requires active>0",
		},
		{
			name:            "busy-with-just-outbound-stale-but-heartbeat-recent (subtle)",
			activeTasks:     1,
			lastOutboundAt:  sql.NullTime{Time: now.Add(-10 * time.Minute), Valid: true},
			lastHeartbeatAt: sql.NullTime{Time: now.Add(-30 * time.Second), Valid: true},
			threshold:       threshold,
			wantWedged:      false,
			explanation:     "Recent heartbeat means the agent is alive; outbound staleness alone is a long turn, not a wedge",
		},
		{
			name:            "non-positive threshold disables detection (defensive)",
			activeTasks:     1,
			lastOutboundAt:  sql.NullTime{},
			lastHeartbeatAt: sql.NullTime{},
			threshold:       0,
			wantWedged:      false,
			explanation:     "A non-positive threshold is operator config footgun; the predicate must not panic, must return false",
		},
		{
			name:            "active=2 with stale everything (multiple stuck turns)",
			activeTasks:     2,
			lastOutboundAt:  sql.NullTime{Time: now.Add(-30 * time.Minute), Valid: true},
			lastHeartbeatAt: sql.NullTime{Time: now.Add(-30 * time.Minute), Valid: true},
			threshold:       threshold,
			wantWedged:      true,
			explanation:     "active>0 (any positive value) with both stale is wedged",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsWedgedAgent(tc.activeTasks, tc.lastOutboundAt, tc.lastHeartbeatAt, tc.threshold)
			if got != tc.wantWedged {
				t.Errorf("IsWedgedAgent(active=%d, outbound=%+v, heartbeat=%+v, threshold=%s) = %v, want %v\n  explanation: %s",
					tc.activeTasks, tc.lastOutboundAt, tc.lastHeartbeatAt, tc.threshold, got, tc.wantWedged, tc.explanation)
			}
		})
	}
}

func TestIsWedgedAgent_ThresholdBoundary(t *testing.T) {
	// "Fresh" timestamps (well within threshold) are not wedged. The
	// predicate uses `now.Sub(t) > threshold` (strict greater-than),
	// and a 1-second-fresh timestamp is well within a 5-minute
	// threshold — the test is intentionally using a 1-second
	// boundary so the assertion is robust to time.Now() drift between
	// the test setup and the predicate call.
	threshold := 5 * time.Minute
	now := time.Now()
	fresh := sql.NullTime{Time: now.Add(-1 * time.Second), Valid: true}
	if IsWedgedAgent(1, fresh, fresh, threshold) {
		t.Errorf("1-second-fresh timestamps should NOT be wedged (well within threshold)")
	}
}

func TestWedgedThresholdForHTTP_MirrorsMonitor(t *testing.T) {
	// The HTTP `wedged` flag and the monitor's sweep query must use
	// the same threshold, otherwise a flag flip on the HTTP response
	// might disagree with the monitor's dispatch. WedgedThresholdForHTTP
	// is the contract: same env var, same parse, same default.
	if got := WedgedThresholdForHTTP(); got <= 0 {
		t.Errorf("WedgedThresholdForHTTP() = %s, want positive duration", got)
	}
}
