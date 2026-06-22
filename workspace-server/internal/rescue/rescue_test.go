package rescue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withFakes swaps the injected RunRemote + Redact for the duration of a
// test and restores them after. Mirrors the provisioner test-fake
// pattern (package-var swap + t.Cleanup).
func withFakes(t *testing.T, run func(ctx context.Context, instanceID, cmd string) (string, error), redact func(ws, c string) string) {
	t.Helper()
	prevRun, prevRedact := RunRemote, Redact
	RunRemote = run
	Redact = redact
	t.Cleanup(func() { RunRemote = prevRun; Redact = prevRedact })
}

// captureLoki points the audit shipper at a temp JSONL file and returns
// a reader that decodes the records the rescue ship() loop wrote. This
// is the same transport the production rescue stream uses (audit.Emit →
// Loki via the tenant Vector source), so asserting on it proves the
// shipper-reuse + labels end to end.
func captureLoki(t *testing.T) func() []map[string]any {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	t.Setenv("MOLECULE_AUDIT_LOG_PATH", path)
	return func() []map[string]any {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("bad audit jsonl line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}
}

func fields(rec map[string]any) map[string]any {
	f, _ := rec["fields"].(map[string]any)
	return f
}

// TestCapture_ShipsAllSectionsWithRescueLabels is the happy path: a
// boot-failure capture collects every fixed section, runs each through
// the redactor, and ships it to Loki under {kind="rescue", org, ws}.
func TestCapture_ShipsAllSectionsWithRescueLabels(t *testing.T) {
	readLoki := captureLoki(t)
	var seenCmds []string
	withFakes(t,
		func(_ context.Context, instanceID, cmd string) (string, error) {
			seenCmds = append(seenCmds, cmd)
			return "OUTPUT for " + instanceID, nil
		},
		func(_ws, c string) string { return c }, // identity redactor
	)

	Capture(context.Background(), Input{
		InstanceID:  "i-abc123",
		WorkspaceID: "ws-1",
		OrgID:       "org-9",
		Reason:      "provision_timeout_sweep",
	})

	recs := readLoki()
	if len(recs) != len(bundleSections) {
		t.Fatalf("want %d shipped sections, got %d", len(bundleSections), len(recs))
	}
	if len(seenCmds) != len(bundleSections) {
		t.Fatalf("want %d remote commands run, got %d", len(bundleSections), len(seenCmds))
	}
	for _, rec := range recs {
		if rec["event_type"] != rescueEventType {
			t.Errorf("event_type = %v, want %q", rec["event_type"], rescueEventType)
		}
		// workspace_id is promoted to the top-level record position by
		// the audit shipper.
		if rec["workspace_id"] != "ws-1" {
			t.Errorf("top-level workspace_id = %v, want ws-1", rec["workspace_id"])
		}
		f := fields(rec)
		if f["kind"] != LokiKind {
			t.Errorf("kind = %v, want %q", f["kind"], LokiKind)
		}
		if f["org"] != "org-9" {
			t.Errorf("org = %v, want org-9", f["org"])
		}
		if f["instance_id"] != "i-abc123" {
			t.Errorf("instance_id = %v, want i-abc123", f["instance_id"])
		}
		if f["redacted"] != true {
			t.Errorf("redacted = %v, want true for a collected section", f["redacted"])
		}
	}
}

// TestCapture_Redacts proves the bundle is scrubbed before it leaves the
// box: a remote section that contains a secret-shaped token must ship
// with the token replaced, never raw.
func TestCapture_Redacts(t *testing.T) {
	readLoki := captureLoki(t)
	const secret = "sk-ant-SUPERSECRETTOKENVALUE0001"
	withFakes(t,
		func(_ context.Context, _ string, _ string) (string, error) {
			return "ANTHROPIC_API_KEY=" + secret, nil
		},
		// redactor that mangles anything containing the secret shape
		func(_ws, c string) string {
			if strings.Contains(c, secret) {
				return strings.ReplaceAll(c, secret, "[REDACTED]")
			}
			return c
		},
	)

	Capture(context.Background(), Input{InstanceID: "i-x", WorkspaceID: "ws-2", OrgID: "o"})

	for _, rec := range readLoki() {
		content, _ := fields(rec)["content"].(string)
		if strings.Contains(content, secret) {
			t.Fatalf("raw secret leaked to Loki in section %v: %q", fields(rec)["section"], content)
		}
	}
}

// TestCapture_SkipsWhenNoInstance: a failure with no provisioned EC2 has
// nothing to read — Capture must no-op (ship nothing) rather than dial a
// blank instance id.
func TestCapture_SkipsWhenNoInstance(t *testing.T) {
	readLoki := captureLoki(t)
	called := false
	withFakes(t,
		func(_ context.Context, _ string, _ string) (string, error) { called = true; return "", nil },
		func(_ws, c string) string { return c },
	)
	Capture(context.Background(), Input{InstanceID: "", WorkspaceID: "ws-3", OrgID: "o"})
	if called {
		t.Error("RunRemote called for an empty instance id")
	}
	if recs := readLoki(); len(recs) != 0 {
		t.Errorf("shipped %d records for an empty instance id, want 0", len(recs))
	}
}

// TestCapture_FailsClosedWithoutRedactor: if the redactor is not wired,
// Capture must NOT ship anything (would leak raw config). Fail closed.
func TestCapture_FailsClosedWithoutRedactor(t *testing.T) {
	readLoki := captureLoki(t)
	prevRun, prevRedact := RunRemote, Redact
	RunRemote = func(_ context.Context, _ string, _ string) (string, error) { return "raw config", nil }
	Redact = nil
	t.Cleanup(func() { RunRemote = prevRun; Redact = prevRedact })

	Capture(context.Background(), Input{InstanceID: "i-x", WorkspaceID: "ws-4", OrgID: "o"})

	if recs := readLoki(); len(recs) != 0 {
		t.Errorf("shipped %d records without a redactor wired, want 0 (fail closed)", len(recs))
	}
}

// TestCapture_SectionFailureIsIsolated: one section's RunRemote error
// must not abort the rest — the failing section ships a marker and the
// others still ship.
func TestCapture_SectionFailureIsIsolated(t *testing.T) {
	readLoki := captureLoki(t)
	withFakes(t,
		func(_ context.Context, _ string, cmd string) (string, error) {
			if strings.Contains(cmd, "config.yaml") {
				return "", errors.New("ssh blip")
			}
			return "ok", nil
		},
		func(_ws, c string) string { return c },
	)

	Capture(context.Background(), Input{InstanceID: "i-x", WorkspaceID: "ws-5", OrgID: "o"})

	recs := readLoki()
	if len(recs) != len(bundleSections) {
		t.Fatalf("want all %d sections shipped (incl. failure marker), got %d", len(bundleSections), len(recs))
	}
	var failureMarkers int
	for _, rec := range recs {
		if fields(rec)["redacted"] == false {
			failureMarkers++
			content, _ := fields(rec)["content"].(string)
			if !strings.Contains(content, "section collection failed") {
				t.Errorf("failure marker content = %q, want a collection-failed marker", content)
			}
		}
	}
	if failureMarkers != 1 {
		t.Errorf("want exactly 1 failure marker, got %d", failureMarkers)
	}
}

// TestCapture_NoWiringIsSafeNoOp: with RunRemote unwired (operator hasn't
// called the boot wiring), Capture must be a logged no-op, never a panic.
func TestCapture_NoWiringIsSafeNoOp(t *testing.T) {
	readLoki := captureLoki(t)
	prevRun, prevRedact := RunRemote, Redact
	RunRemote = nil
	Redact = func(_ws, c string) string { return c }
	t.Cleanup(func() { RunRemote = prevRun; Redact = prevRedact })

	Capture(context.Background(), Input{InstanceID: "i-x", WorkspaceID: "ws-6", OrgID: "o"})

	if recs := readLoki(); len(recs) != 0 {
		t.Errorf("shipped %d records with RunRemote unwired, want 0", len(recs))
	}
}
