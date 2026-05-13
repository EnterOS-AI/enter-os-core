package handlers

// a2a_queue_expiry_test.go — unit coverage for extractExpiresInSeconds
// (a2a_queue.go). Tests the pure TTL-extraction logic used by the
// heartbeat drain path when enqueuing a message with a caller-specified TTL.
// Priority constants ordering is also covered here so the a2a_queue.go
// package has complete pure-function coverage.

import "testing"

// ─── extractExpiresInSeconds ────────────────────────────────────────────────

func TestExtractExpiresInSeconds_Valid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"positive int", `{"params":{"expires_in_seconds":30}}`, 30},
		{"zero", `{"params":{"expires_in_seconds":0}}`, 0},
		{"large TTL", `{"params":{"expires_in_seconds":3600}}`, 3600},
		{"nested message unaffected", `{"params":{"message":{"role":"user"},"expires_in_seconds":60}}`, 60},
		{"float truncated", `{"params":{"expires_in_seconds":90.7}}`, 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractExpiresInSeconds([]byte(tc.body))
			if got != tc.want {
				t.Errorf("extractExpiresInSeconds(%q) = %d; want %d", tc.body, got, tc.want)
			}
		})
	}
}

func TestExtractExpiresInSeconds_InvalidOrMissing(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"negative → 0", `{"params":{"expires_in_seconds":-5}}`, 0},
		{"missing params", `{}`, 0},
		{"missing expires_in_seconds", `{"params":{"message":"hello"}}`, 0},
		{"malformed JSON", `"not json at all`, 0},
		{"null body", `null`, 0},
		{"empty string", ``, 0},
		{"wrong type string", `{"params":{"expires_in_seconds":"30"}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractExpiresInSeconds([]byte(tc.body))
			if got != tc.want {
				t.Errorf("extractExpiresInSeconds(%q) = %d; want %d", tc.body, got, tc.want)
			}
		})
	}
}

// ─── Priority constants ────────────────────────────────────────────────────

func TestPriorityConstants_Ordering(t *testing.T) {
	// The ordering invariant: Critical > Task > Info.
	// These constants govern queue drain priority — if ordering is wrong,
	// high-priority items get starved.
	if PriorityCritical <= PriorityTask {
		t.Errorf("PriorityCritical(%d) must be > PriorityTask(%d)", PriorityCritical, PriorityTask)
	}
	if PriorityTask <= PriorityInfo {
		t.Errorf("PriorityTask(%d) must be > PriorityInfo(%d)", PriorityTask, PriorityInfo)
	}
	if PriorityCritical <= PriorityInfo {
		t.Errorf("PriorityCritical(%d) must be > PriorityInfo(%d)", PriorityCritical, PriorityInfo)
	}
}

func TestPriorityConstants_Values(t *testing.T) {
	// Pin the values so callers can rely on them for queue inspection
	// and admin endpoints without re-reading the source.
	if PriorityCritical != 100 {
		t.Errorf("PriorityCritical = %d; want 100", PriorityCritical)
	}
	if PriorityTask != 50 {
		t.Errorf("PriorityTask = %d; want 50", PriorityTask)
	}
	if PriorityInfo != 10 {
		t.Errorf("PriorityInfo = %d; want 10", PriorityInfo)
	}
}
