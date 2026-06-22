package handlers

// template_files_eic_write_timeout_test.go — pins the actionable-error
// behavior added for internal#423.
//
// When the per-op context deadline (eicFileOpTimeout) fires,
// exec.CommandContext SIGKILLs the ssh subprocess and Run() returns the
// bare "signal: killed" with empty stderr. Before the fix that surfaced
// to the canvas as an opaque `500 {"error":"ssh install: signal:
// killed ()"}` — useless to an operator whose workspace was simply
// mid-provision with a slow/unready EIC tunnel. The fix detects the
// deadline explicitly (errors.Is(ctx.Err(), context.DeadlineExceeded))
// and returns a message that names the cause and the
// Settings → Secrets workaround.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestWriteFileViaEIC_DeadlineExceeded_ActionableError stubs
// withEICTunnel so the *real* inner closure runs against a context that
// has already exceeded its deadline. The ssh subprocess fails (no real
// sshd on the fake port) and ctx.Err() == DeadlineExceeded, so the new
// branch must fire and produce an actionable message — NOT the opaque
// "signal: killed ()" string the canvas used to show.
func TestWriteFileViaEIC_DeadlineExceeded_ActionableError(t *testing.T) {
	prev := withEICTunnel
	withEICTunnel = func(_ context.Context, instanceID string, fn func(s eicSSHSession) error) error {
		// Run the real inner closure. It closes over the ctx that
		// writeFileViaEIC derived from our already-cancelled parent, so
		// the ssh subprocess is killed immediately and ctx.Err()
		// resolves — exactly the eicFileOpTimeout-expiry shape.
		return fn(eicSSHSession{
			instanceID: instanceID,
			osUser:     "ubuntu",
			localPort:  1, // nothing listening → ssh fails fast
			keyPath:    "/nonexistent/key",
		})
	}
	t.Cleanup(func() { withEICTunnel = prev })

	// Drive the real writeFileViaEIC. Pass a parent whose deadline has
	// already passed: the context.WithTimeout(ctx, eicFileOpTimeout)
	// derived inside writeFileViaEIC inherits the expired parent
	// deadline, so ctx.Err() == context.DeadlineExceeded by the time
	// the killed ssh subprocess returns — the exact production shape
	// (eicFileOpTimeout expiry), exercised deterministically.
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := writeFileViaEIC(parent, "i-test", "claude-code", "/configs", "config.yaml", []byte("model: sonnet\n"))
	if err == nil {
		t.Fatalf("expected an error from a killed ssh subprocess, got nil")
	}
	msg := err.Error()

	// Must NOT leak the opaque bare-signal string to the operator.
	if strings.Contains(msg, "signal: killed ()") {
		t.Fatalf("error still surfaces the opaque %q form: %q", "signal: killed ()", msg)
	}
	// Must name the cause and the Secrets workaround so the canvas
	// shows something actionable.
	for _, want := range []string{"timed out", "provisioning", "Settings", "Secrets"} {
		if !strings.Contains(msg, want) {
			t.Errorf("actionable error missing %q; got: %q", want, msg)
		}
	}
}
