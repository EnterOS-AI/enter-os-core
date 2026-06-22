package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTopLevelRuntime(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"top-level claude-code", "name: x\nruntime: claude-code\ntier: 2\n", "claude-code"},
		{"top-level google-adk", "runtime: google-adk\n", "google-adk"},
		{"quoted value", `runtime: "google-adk"` + "\n", "google-adk"},
		{"single-quoted value", "runtime: 'codex'\n", "codex"},
		{"ignores runtime_config nested model", "runtime: google-adk\nruntime_config:\n  model: vertex:gemini-2.5-pro\n", "google-adk"},
		{"runtime_config only, no top-level runtime", "name: y\nruntime_config:\n  model: x\n", ""},
		{"indented runtime is not top-level", "wrapper:\n  runtime: claude-code\n", ""},
		{"empty", "", ""},
		{"no runtime key", "name: z\ntier: 4\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseTopLevelRuntime([]byte(tc.yaml)); got != tc.want {
				t.Fatalf("parseTopLevelRuntime(%q) = %q, want %q", tc.yaml, got, tc.want)
			}
		})
	}
}

func TestSeededConfigRuntime(t *testing.T) {
	// in-memory configFiles wins over template dir.
	t.Run("from configFiles", func(t *testing.T) {
		cf := map[string][]byte{"config.yaml": []byte("runtime: google-adk\n")}
		if got := seededConfigRuntime("/nonexistent", cf); got != "google-adk" {
			t.Fatalf("got %q, want google-adk", got)
		}
	})

	// falls back to template dir's config.yaml.
	t.Run("from template dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("name: a\nruntime: claude-code\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := seededConfigRuntime(dir, nil); got != "claude-code" {
			t.Fatalf("got %q, want claude-code", got)
		}
	})

	// nothing available → "".
	t.Run("indeterminate", func(t *testing.T) {
		if got := seededConfigRuntime("", nil); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
		if got := seededConfigRuntime("/does/not/exist", map[string][]byte{}); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestRuntimeSeedMismatchAbort(t *testing.T) {
	adkCfg := map[string][]byte{"config.yaml": []byte("runtime: google-adk\n")}
	ccCfg := map[string][]byte{"config.yaml": []byte("name: Claude Code Agent\nruntime: claude-code\n")}

	t.Run("mismatch fails loud (the #2027 demo bug)", func(t *testing.T) {
		// requested google-adk, but seeding the claude-code-default config.
		abort := runtimeSeedMismatchAbort("google-adk", "", ccCfg)
		if abort == nil {
			t.Fatal("expected abort for google-adk requested but claude-code seeded, got nil")
		}
		if abort.Extra["requested_runtime"] != "google-adk" || abort.Extra["seeded_runtime"] != "claude-code" {
			t.Fatalf("abort.Extra mismatch: %+v", abort.Extra)
		}
		if abort.Extra["issue"] != "2027" {
			t.Fatalf("expected issue 2027 tag, got %v", abort.Extra["issue"])
		}
	})

	t.Run("match is allowed", func(t *testing.T) {
		if abort := runtimeSeedMismatchAbort("google-adk", "", adkCfg); abort != nil {
			t.Fatalf("expected no abort when seeded runtime matches, got %q", abort.Msg)
		}
	})

	t.Run("empty requested runtime is allowed (org-template default path)", func(t *testing.T) {
		if abort := runtimeSeedMismatchAbort("", "", ccCfg); abort != nil {
			t.Fatalf("expected no abort for unspecified runtime, got %q", abort.Msg)
		}
	})

	t.Run("indeterminate seed is allowed (CP mode, no local config bytes)", func(t *testing.T) {
		if abort := runtimeSeedMismatchAbort("google-adk", "", nil); abort != nil {
			t.Fatalf("expected no abort when seeded runtime is indeterminate, got %q", abort.Msg)
		}
	})

	t.Run("mismatch via template dir also fails loud", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("runtime: claude-code\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if abort := runtimeSeedMismatchAbort("hermes", dir, nil); abort == nil {
			t.Fatal("expected abort for hermes requested but claude-code template seeded")
		}
	})
}
