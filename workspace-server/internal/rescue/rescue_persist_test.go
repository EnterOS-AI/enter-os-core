package rescue

// Part 3 coverage: Capture, after collecting + redacting every section,
// persists the bundle exactly once to the queryable store (in addition
// to the per-section Loki ship verified in rescue_test.go).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// withPersist swaps the injected PersistBundle for the test and restores
// it after.
func withPersist(t *testing.T, fn func(ctx context.Context, b Bundle) error) {
	t.Helper()
	prev := PersistBundle
	PersistBundle = fn
	t.Cleanup(func() { PersistBundle = prev })
}

// TestCapture_PersistsBundleOnce: the happy path persists one bundle
// carrying every section, with identity + redacted content matching what
// was shipped.
func TestCapture_PersistsBundleOnce(t *testing.T) {
	_ = captureLoki(t) // keep Loki transport pointed at a temp file
	withFakes(t,
		func(_ context.Context, instanceID, cmd string) (string, error) {
			return "OUT:" + instanceID, nil
		},
		func(_ws, c string) string { return "RED:" + c },
	)

	var persisted []Bundle
	withPersist(t, func(_ context.Context, b Bundle) error {
		persisted = append(persisted, b)
		return nil
	})

	Capture(context.Background(), Input{
		InstanceID:  "i-abc",
		WorkspaceID: "ws-1",
		OrgID:       "org-9",
		Reason:      "provision_timeout_sweep",
	})

	if len(persisted) != 1 {
		t.Fatalf("PersistBundle called %d times, want exactly 1", len(persisted))
	}
	b := persisted[0]
	if b.WorkspaceID != "ws-1" || b.OrgID != "org-9" || b.InstanceID != "i-abc" || b.Reason != "provision_timeout_sweep" {
		t.Errorf("bundle identity wrong: %+v", b)
	}
	if len(b.Sections) != len(bundleSections) {
		t.Fatalf("persisted %d sections, want %d", len(b.Sections), len(bundleSections))
	}
	for _, s := range b.Sections {
		if !s.Redacted {
			t.Errorf("section %q persisted with redacted=false on the happy path", s.Name)
		}
		// Redactor ("RED:" prefix) must have run on persisted content.
		if !strings.HasPrefix(s.Content, "RED:") {
			t.Errorf("section %q persisted un-redacted content: %q", s.Name, s.Content)
		}
	}
}

// TestCapture_PersistFailureDoesNotPanic: a store error is swallowed —
// Capture still completes (the Loki ship already succeeded).
func TestCapture_PersistFailureDoesNotPanic(t *testing.T) {
	_ = captureLoki(t)
	withFakes(t,
		func(_ context.Context, _ string, _ string) (string, error) { return "ok", nil },
		func(_ws, c string) string { return c },
	)
	withPersist(t, func(_ context.Context, _ Bundle) error {
		return errors.New("db down")
	})
	// Must not panic / must return normally.
	Capture(context.Background(), Input{InstanceID: "i-x", WorkspaceID: "ws-2", OrgID: "o"})
}

// TestCapture_NoPersistWiredIsSafe: with PersistBundle unwired (operator
// hasn't wired the read path), Capture still ships to Loki and does not
// panic.
func TestCapture_NoPersistWiredIsSafe(t *testing.T) {
	readLoki := captureLoki(t)
	withFakes(t,
		func(_ context.Context, _ string, _ string) (string, error) { return "ok", nil },
		func(_ws, c string) string { return c },
	)
	prev := PersistBundle
	PersistBundle = nil
	t.Cleanup(func() { PersistBundle = prev })

	Capture(context.Background(), Input{InstanceID: "i-x", WorkspaceID: "ws-3", OrgID: "o"})

	// Loki ship still happened for every section.
	if recs := readLoki(); len(recs) != len(bundleSections) {
		t.Errorf("shipped %d records, want %d (Loki unaffected by missing store)", len(recs), len(bundleSections))
	}
}

// TestCapture_FailureMarkerPersistedAsNonRedacted: a section whose
// collection fails is persisted with redacted=false + a marker, matching
// the Loki record.
func TestCapture_FailureMarkerPersistedAsNonRedacted(t *testing.T) {
	_ = captureLoki(t)
	withFakes(t,
		func(_ context.Context, _ string, cmd string) (string, error) {
			if strings.Contains(cmd, "config.yaml") {
				return "", errors.New("ssh blip")
			}
			return "ok", nil
		},
		func(_ws, c string) string { return c },
	)
	var got Bundle
	withPersist(t, func(_ context.Context, b Bundle) error { got = b; return nil })

	Capture(context.Background(), Input{InstanceID: "i-x", WorkspaceID: "ws-4", OrgID: "o"})

	var markers int
	for _, s := range got.Sections {
		if !s.Redacted {
			markers++
			if !strings.Contains(s.Content, "section collection failed") {
				t.Errorf("non-redacted section %q content = %q, want a failure marker", s.Name, s.Content)
			}
		}
	}
	if markers != 1 {
		t.Errorf("want exactly 1 failure marker persisted, got %d", markers)
	}
}
