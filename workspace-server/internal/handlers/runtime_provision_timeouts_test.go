package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTemplate is a tiny test fixture: drop a config.yaml under
// tmp/<dir>/config.yaml with the given content. Mirrors the real
// configsDir layout (one subdir per template, each with its own
// config.yaml).
func writeTemplate(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	if err := os.WriteFile(filepath.Join(p, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func TestLoadRuntimeProvisionTimeouts_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "template-hermes", `
name: Hermes
runtime: hermes
runtime_config:
  provision_timeout_seconds: 720
`)
	writeTemplate(t, dir, "template-claude-code", `
name: Claude
runtime: claude-code
runtime_config:
  model: anthropic:claude-opus
`)
	got := loadRuntimeProvisionTimeouts(dir)
	if got["hermes"] != 720 {
		t.Errorf("hermes: got %d, want 720", got["hermes"])
	}
	// claude-code didn't declare a timeout — must not appear in the map
	// (zero-value lookup is the no-override signal).
	if _, ok := got["claude-code"]; ok {
		t.Errorf("claude-code: present without declaration: %d", got["claude-code"])
	}
}

func TestLoadRuntimeProvisionTimeouts_MaxOnDuplicateRuntime(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "template-hermes-fast", `
runtime: hermes
runtime_config:
  provision_timeout_seconds: 300
`)
	writeTemplate(t, dir, "template-hermes-slow", `
runtime: hermes
runtime_config:
  provision_timeout_seconds: 900
`)
	got := loadRuntimeProvisionTimeouts(dir)
	// Max wins so the slowest template's threshold doesn't false-alarm
	// when both templates use the same runtime.
	if got["hermes"] != 900 {
		t.Errorf("max-on-duplicate: got %d, want 900", got["hermes"])
	}
}

func TestLoadRuntimeProvisionTimeouts_SkipsBadInputs(t *testing.T) {
	dir := t.TempDir()
	// Missing runtime field — has timeout but no key to map under.
	writeTemplate(t, dir, "template-no-runtime", `
runtime_config:
  provision_timeout_seconds: 600
`)
	// Zero/negative timeout — same as no declaration.
	writeTemplate(t, dir, "template-zero", `
runtime: zero-runtime
runtime_config:
  provision_timeout_seconds: 0
`)
	// Malformed yaml — must not crash.
	writeTemplate(t, dir, "template-bad", "not: valid: yaml: at: all:")
	// Loose file at the top level (not a dir) — must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadRuntimeProvisionTimeouts(dir)
	if len(got) != 0 {
		t.Errorf("expected empty map for skip cases, got %v", got)
	}
}

func TestLoadRuntimeProvisionTimeouts_MissingDirReturnsEmpty(t *testing.T) {
	got := loadRuntimeProvisionTimeouts("/nonexistent/path/should/not/exist/12345")
	if len(got) != 0 {
		t.Errorf("expected empty map on missing dir, got %v", got)
	}
}

func TestRuntimeProvisionTimeoutsCache_LazyInitAndCached(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "template-hermes", `
runtime: hermes
runtime_config:
  provision_timeout_seconds: 720
`)
	c := runtimeProvisionTimeoutsCache{}

	// First call populates.
	if got := c.get(dir, "hermes"); got != 720 {
		t.Errorf("first call: got %d, want 720", got)
	}
	// Second call hits cache — even if the underlying file changed we
	// still see the original value (sync.Once contract).
	if err := os.WriteFile(filepath.Join(dir, "template-hermes", "config.yaml"),
		[]byte("runtime: hermes\nruntime_config:\n  provision_timeout_seconds: 60\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.get(dir, "hermes"); got != 720 {
		t.Errorf("cached call: got %d, want 720 (cache must not re-read)", got)
	}
	// Unknown runtime returns zero — caller's signal to fall through to
	// the canvas runtime profile default.
	if got := c.get(dir, "unknown"); got != 0 {
		t.Errorf("unknown runtime: got %d, want 0", got)
	}
}
