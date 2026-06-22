package handlers

import (
	"testing"
)

// Tests for resolveProvisionConcurrency — the env-parse contract that
// turns MOLECULE_PROVISION_CONCURRENCY into the channel-buffer size for
// the org-import provision semaphore.
//
// Why this matters: with the wrong cap, org-import either serializes
// (cap=1, slow) or stampedes the provider (cap=infinity on a backend
// that can't take it). The defaults — 3 for Docker, "0=unlimited" for
// EC2/SaaS — are what most operators want; the parse logic exists to
// route the env var to the right behavior without surprise.
//
// The "0 → unlimited" mapping is the user-facing piece worth pinning
// in tests: easy to misread as "0 means stop entirely" if someone
// re-reads the constant block years later.

func TestResolveProvisionConcurrency_UnsetUsesDefault(t *testing.T) {
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "")
	if got := resolveProvisionConcurrency(); got != defaultProvisionConcurrency {
		t.Errorf("unset env: got %d, want %d", got, defaultProvisionConcurrency)
	}
}

func TestResolveProvisionConcurrency_ZeroIsUnlimited(t *testing.T) {
	// "0" is the user-facing shorthand for "no cap". The implementation
	// returns a large but finite cap so the channel-based semaphore
	// stays a no-op without infinite-buffer risk.
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "0")
	got := resolveProvisionConcurrency()
	if got <= defaultProvisionConcurrency {
		t.Errorf("0 should map to large 'unlimited' cap, got %d", got)
	}
	// 1<<20 today; pin the lower bound rather than the exact value so
	// future tuning of the magic number doesn't break this test.
	if got < 1024 {
		t.Errorf("0 should map to a cap >= 1024 (effectively unlimited), got %d", got)
	}
}

func TestResolveProvisionConcurrency_PositiveIntegerExact(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"1", 1},
		{"5", 5},
		{"10", 10},
		{"50", 50},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("MOLECULE_PROVISION_CONCURRENCY", tc.env)
			if got := resolveProvisionConcurrency(); got != tc.want {
				t.Errorf("env=%q: got %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

func TestResolveProvisionConcurrency_NegativeFallsBackToDefault(t *testing.T) {
	// Negative values are operator misconfiguration. Fall back to the
	// safe default rather than passing through to make(chan, -5) which
	// panics. The handler logs a warning so the operator notices.
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "-5")
	if got := resolveProvisionConcurrency(); got != defaultProvisionConcurrency {
		t.Errorf("negative env: got %d, want default %d", got, defaultProvisionConcurrency)
	}
}

func TestResolveProvisionConcurrency_NonNumericFallsBackToDefault(t *testing.T) {
	// Garbage in env shouldn't crash org-import. Common in dev when an
	// operator types `MOLECULE_PROVISION_CONCURRENCY=true` or similar.
	cases := []string{"true", "yes", "infinity", "ten", "3.5", "0x10"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("MOLECULE_PROVISION_CONCURRENCY", raw)
			if got := resolveProvisionConcurrency(); got != defaultProvisionConcurrency {
				t.Errorf("non-numeric env=%q: got %d, want default %d",
					raw, got, defaultProvisionConcurrency)
			}
		})
	}
}

func TestResolveProvisionConcurrency_WhitespaceTrimmed(t *testing.T) {
	// Operators frequently set env vars with stray whitespace from
	// copy-paste. Trim before parse so " 7 " == "7".
	t.Setenv("MOLECULE_PROVISION_CONCURRENCY", "  7  ")
	if got := resolveProvisionConcurrency(); got != 7 {
		t.Errorf("whitespace env: got %d, want 7", got)
	}
}
