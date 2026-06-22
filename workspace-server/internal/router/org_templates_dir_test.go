package router

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindOrgDir_EnvOverride covers the ORG_TEMPLATES_DIR SSOT override that
// fixes the self-host "No org templates" shadowing bug: the tenant image bakes
// default org templates at /org-templates, and ORG_TEMPLATES_DIR points
// findOrgDir at that baked path so the local stack lists the same defaults
// production ships (instead of an empty host bind-mount shadowing them).
func TestFindOrgDir_EnvOverride(t *testing.T) {
	t.Run("honors ORG_TEMPLATES_DIR when it is a real directory", func(t *testing.T) {
		baked := t.TempDir()
		t.Setenv("ORG_TEMPLATES_DIR", baked)

		got := findOrgDir("/configs")

		absBaked, _ := filepath.Abs(baked)
		if got != absBaked {
			t.Errorf("findOrgDir() = %q, want %q (the ORG_TEMPLATES_DIR path)", got, absBaked)
		}
	})

	t.Run("falls through to discovery when ORG_TEMPLATES_DIR is not a directory", func(t *testing.T) {
		t.Setenv("ORG_TEMPLATES_DIR", filepath.Join(t.TempDir(), "does-not-exist"))

		// With no discoverable org-templates dir on disk relative to cwd, it
		// returns the bare "org-templates" fallback rather than the bad env path.
		got := findOrgDir(filepath.Join(t.TempDir(), "configs"))
		if got == "" {
			t.Fatalf("findOrgDir() returned empty string")
		}
		if filepath.IsAbs(got) {
			// An absolute result means a discovery candidate existed (cwd has an
			// org-templates dir, e.g. when run from repo root). That's fine — the
			// key assertion is it did NOT return the non-existent env path.
			if _, err := os.Stat(got); err != nil {
				t.Errorf("findOrgDir() = %q which does not exist on disk", got)
			}
		}
	})

	t.Run("ignores empty ORG_TEMPLATES_DIR (discovery path)", func(t *testing.T) {
		t.Setenv("ORG_TEMPLATES_DIR", "")
		got := findOrgDir("/configs")
		if got == "" {
			t.Errorf("findOrgDir() returned empty string with ORG_TEMPLATES_DIR unset")
		}
	})
}
