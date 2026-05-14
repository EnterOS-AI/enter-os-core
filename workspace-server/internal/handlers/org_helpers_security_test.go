package handlers

import (
	"path/filepath"
	"strings"
	"testing"
)

// org_helpers_security_test.go — security-critical path sanitization + role-name
// validation for org template processing. Covers OFFSEC-006-class attacks:
// path traversal via user-controlled files_dir / prompt_file refs, and role-name
// injection via the persona env loader.

// ── resolveInsideRoot ──────────────────────────────────────────────────────────

func TestResolveInsideRoot_EmptyUserPath(t *testing.T) {
	_, err := resolveInsideRoot("/safe/root", "")
	if err == nil {
		t.Fatalf("empty userPath: expected error, got nil")
	}
	if err.Error() != "path is empty" {
		t.Errorf("empty userPath: got %q, want %q", err.Error(), "path is empty")
	}
}

func TestResolveInsideRoot_AbsolutePathRejected(t *testing.T) {
	_, err := resolveInsideRoot("/safe/root", "/etc/passwd")
	if err == nil {
		t.Fatalf("absolute userPath: expected error, got nil")
	}
	if err.Error() != "absolute paths are not allowed" {
		t.Errorf("absolute userPath: got %q, want %q", err.Error(), "absolute paths are not allowed")
	}
}

func TestResolveInsideRoot_DotDotTraversal(t *testing.T) {
	// ../../etc/passwd from /safe/root
	got, err := resolveInsideRoot("/safe/root", "../../etc/passwd")
	if err == nil {
		t.Fatalf("dotdot traversal: expected error, got %q", got)
	}
	if err.Error() != "path escapes root" {
		t.Errorf("dotdot traversal: got %q, want %q", err.Error(), "path escapes root")
	}
}

// TestResolveInsideRoot_DotDotWithIntermediate verifies that a/b/../../c does NOT
// escape when root=/safe/root. After normalization: a/b/../.. = ., so a/b/../../c = c,
// which is a valid descendant of /safe/root. The original test expected an error
// but resolveInsideRoot correctly returns nil (the path stays within root).
// The OFFSEC-006 concern is covered by ../../etc/passwd which DOES escape.
func TestResolveInsideRoot_DotDotWithIntermediate(t *testing.T) {
	root := t.TempDir()
	got, err := resolveInsideRoot(root, "a/b/../../c")
	if err != nil {
		t.Fatalf("a/b/../../c should resolve (normalizes to c within root): %v", err)
	}
	if !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Errorf("result should be inside root %q, got %q", root, got)
	}
	// Ensure the suffix is "c"
	parts := strings.Split(strings.TrimPrefix(got, root), string(filepath.Separator))
	if parts[len(parts)-1] != "c" {
		t.Errorf("expected filename 'c', got %q", got)
	}
}

func TestResolveInsideRoot_ValidRelativePath(t *testing.T) {
	// This test uses the real filesystem since resolveInsideRoot calls filepath.Abs.
	// Use t.TempDir() so we have a real root to work with.
	root := t.TempDir()
	got, err := resolveInsideRoot(root, "subdir/file.txt")
	if err != nil {
		t.Fatalf("valid relative: unexpected error: %v", err)
	}
	// Must be inside root
	if got[:len(root)] != root {
		t.Errorf("result should start with root %q, got %q", root, got)
	}
}

func TestResolveInsideRoot_ExactRootMatch(t *testing.T) {
	root := t.TempDir()
	got, err := resolveInsideRoot(root, ".")
	if err != nil {
		t.Fatalf("exact root: unexpected error: %v", err)
	}
	if got != root {
		t.Errorf("exact root match: got %q, want %q", got, root)
	}
}

func TestResolveInsideRoot_DotPathComponent(t *testing.T) {
	root := t.TempDir()
	// ./subdir/./file.txt should resolve to root/subdir/file.txt
	got, err := resolveInsideRoot(root, "./subdir/./file.txt")
	if err != nil {
		t.Fatalf("dot path component: unexpected error: %v", err)
	}
	// Verify the file component is subdir/file.txt regardless of root length.
	suffix := string(filepath.Separator) + "subdir" + string(filepath.Separator) + "file.txt"
	if !strings.HasSuffix(got, suffix) {
		t.Errorf("dot path component: got %q, want suffix %q", got, suffix)
	}
}

func TestResolveInsideRoot_NestedDotDotEscapes(t *testing.T) {
	root := t.TempDir()
	// a/../../b from /tmp/xyz → /tmp/b (escapes temp dir)
	got, err := resolveInsideRoot(root, "a/../../b")
	if err == nil {
		t.Fatalf("nested dotdot: expected error, got %q", got)
	}
	if err.Error() != "path escapes root" {
		t.Errorf("nested dotdot: got %q, want %q", err.Error(), "path escapes root")
	}
}

func TestResolveInsideRoot_DotdotAtStart(t *testing.T) {
	root := t.TempDir()
	got, err := resolveInsideRoot(root, "../sibling")
	if err == nil {
		t.Fatalf("../sibling: expected error, got %q", got)
	}
	if err.Error() != "path escapes root" {
		t.Errorf("../sibling: got %q, want %q", err.Error(), "path escapes root")
	}
}

func TestResolveInsideRoot_SiblingNotEscaped(t *testing.T) {
	// /foo/bar and /foo/baz are siblings — the prefix check with
	// filepath.Separator guard must allow /foo/bar/child without matching /foo/baz
	// (which would be wrong if the check were just strings.HasPrefix).
	root := t.TempDir()
	got, err := resolveInsideRoot(root, "valid-subdir/file.txt")
	if err != nil {
		t.Fatalf("sibling not escaped: unexpected error: %v", err)
	}
	// Must be inside root
	if !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Errorf("result should be inside root %q, got %q", root, got)
	}
}

// ── isSafeRoleName ────────────────────────────────────────────────────────────
// isSafeRoleName is tested comprehensively in org_helpers_pure_test.go.
// Only security-critical path-injection cases live here.

// ── mergeCategoryRouting ──────────────────────────────────────────────────────
// Duplicate mergeCategoryRouting tests removed to avoid redeclaration with
// org_helpers_pure_test.go. Only security-specific behaviour lives here.

func TestSecureRouting_BothNil(t *testing.T) {
	got := mergeCategoryRouting(nil, nil)
	if len(got) != 0 {
		t.Errorf("both nil: got %v, want empty", got)
	}
}

func TestSecureRouting_DefaultOnly(t *testing.T) {
	defaultRouting := map[string][]string{
		"security": {"Backend Engineer", "DevOps"},
	}
	got := mergeCategoryRouting(defaultRouting, nil)
	if len(got) != 1 {
		t.Fatalf("default only: got %d entries, want 1", len(got))
	}
	if len(got["security"]) != 2 {
		t.Errorf("security roles: got %v, want [Backend Engineer, DevOps]", got["security"])
	}
}

func TestSecureRouting_WorkspaceOnly(t *testing.T) {
	wsRouting := map[string][]string{
		"ui": {"Frontend Engineer"},
	}
	got := mergeCategoryRouting(nil, wsRouting)
	if len(got) != 1 {
		t.Fatalf("ws only: got %d entries, want 1", len(got))
	}
	if got["ui"][0] != "Frontend Engineer" {
		t.Errorf("ui roles: got %v, want [Frontend Engineer]", got["ui"])
	}
}

func TestSecureRouting_MergeNoOverlap(t *testing.T) {
	defaultRouting := map[string][]string{
		"security": {"Backend Engineer"},
	}
	wsRouting := map[string][]string{
		"ui": {"Frontend Engineer"},
	}
	got := mergeCategoryRouting(defaultRouting, wsRouting)
	if len(got) != 2 {
		t.Errorf("merge no overlap: got %d entries, want 2", len(got))
	}
}

func TestSecureRouting_WsOverrideDropsDefault(t *testing.T) {
	defaultRouting := map[string][]string{
		"security": {"Backend Engineer", "DevOps"},
	}
	wsRouting := map[string][]string{
		"security": {"Security Engineer"},
	}
	got := mergeCategoryRouting(defaultRouting, wsRouting)
	if len(got["security"]) != 1 {
		t.Errorf("ws override: got %v, want [Security Engineer]", got["security"])
	}
	if got["security"][0] != "Security Engineer" {
		t.Errorf("ws override: got %v, want [Security Engineer]", got["security"])
	}
}

func TestSecureRouting_EmptyListDropsCategory(t *testing.T) {
	defaultRouting := map[string][]string{
		"security": {"Backend Engineer"},
		"ui":       {"Frontend Engineer"},
	}
	wsRouting := map[string][]string{
		"security": {}, // empty list = opt out
	}
	got := mergeCategoryRouting(defaultRouting, wsRouting)
	if _, exists := got["security"]; exists {
		t.Error("empty ws list should delete the category from output")
	}
	if len(got["ui"]) != 1 {
		t.Errorf("ui should still exist: got %v", got["ui"])
	}
}

func TestSecureRouting_EmptyKeySkipped(t *testing.T) {
	defaultRouting := map[string][]string{
		"": {"Backend Engineer"},
	}
	got := mergeCategoryRouting(defaultRouting, nil)
	if _, exists := got[""]; exists {
		t.Error("empty key should be skipped")
	}
}

func TestSecureRouting_EmptyRolesInDefaultSkipped(t *testing.T) {
	defaultRouting := map[string][]string{
		"security": {},
	}
	got := mergeCategoryRouting(defaultRouting, nil)
	if len(got) != 0 {
		t.Errorf("empty roles in default should be skipped, got %v", got)
	}
}

func TestSecureRouting_OriginalMapsUnmodified(t *testing.T) {
	defaultRouting := map[string][]string{
		"security": {"Backend Engineer"},
	}
	wsRouting := map[string][]string{
		"ui": {"Frontend Engineer"},
	}
	mergeCategoryRouting(defaultRouting, wsRouting)
	if len(defaultRouting) != 1 || len(defaultRouting["security"]) != 1 {
		t.Error("default routing should be unmodified after merge")
	}
	if len(wsRouting) != 1 {
		t.Error("ws routing should be unmodified after merge")
	}
}

// ── expandWithEnv ─────────────────────────────────────────────────────────────
//
// CWE-78 regression tests. The original fix (a3a358f9) ensures that partial
// variable references like $HOME/path are NOT resolved via os.Getenv — the
// host HOME env var must not leak into org template values. Only whole-string
// references ($VAR or ${VAR}) may fall back to the host process environment.

func TestExpandWithEnv_PartialRefDollarHomePath(t *testing.T) {
	// $HOME/path must NOT resolve to the host's HOME env var.
	// The literal $HOME must be returned as-is.
	got := expandWithEnv("$HOME/path", nil)
	if got != "$HOME/path" {
		t.Errorf("$HOME/path: got %q, want literal $HOME/path", got)
	}
}

func TestExpandWithEnv_PartialRefBracedRoleAdmin(t *testing.T) {
	// ${ROLE}/admin — ROLE is not in env, so expand to the literal ${ROLE}/admin.
	got := expandWithEnv("${ROLE}/admin", nil)
	if got != "${ROLE}/admin" {
		t.Errorf("${ROLE}/admin: got %q, want literal ${ROLE}/admin", got)
	}
}

func TestExpandWithEnv_PartialRefMiddleOfString(t *testing.T) {
	// $ROLE in the middle of a string — literal, not os.Getenv.
	got := expandWithEnv("prefix/$ROLE/suffix", nil)
	if got != "prefix/$ROLE/suffix" {
		t.Errorf("prefix/$ROLE/suffix: got %q, want literal", got)
	}
}

func TestExpandWithEnv_WholeVarInEnv(t *testing.T) {
	// Whole-string $VAR that IS in env — env value wins.
	env := map[string]string{"FOO": "barvalue"}
	got := expandWithEnv("$FOO", env)
	if got != "barvalue" {
		t.Errorf("$FOO with FOO=barvalue: got %q, want barvalue", got)
	}
}

func TestExpandWithEnv_WholeVarBracedInEnv(t *testing.T) {
	// Whole-string ${VAR} that IS in env — env value wins.
	env := map[string]string{"FOO": "barvalue"}
	got := expandWithEnv("${FOO}", env)
	if got != "barvalue" {
		t.Errorf("${FOO} with FOO=barvalue: got %q, want barvalue", got)
	}
}

func TestExpandWithEnv_WholeVarNotInEnvBare(t *testing.T) {
	// Whole-string $VAR not in env — falls back to os.Getenv.
	// If the host has the var, we get the host value. If not, empty.
	// At minimum, the result must NOT be the literal "$UNDEFINED_VAR_9Z".
	got := expandWithEnv("$UNDEFINED_VAR_9Z", nil)
	if got == "$UNDEFINED_VAR_9Z" {
		t.Errorf("$UNDEFINED_VAR_9Z: should expand (whole-string fallback to os.Getenv), got literal")
	}
}

func TestExpandWithEnv_WholeVarNotInEnvBraced(t *testing.T) {
	// Whole-string ${VAR} not in env — falls back to os.Getenv.
	got := expandWithEnv("${UNDEFINED_VAR_9Z}", nil)
	if got == "${UNDEFINED_VAR_9Z}" {
		t.Errorf("${UNDEFINED_VAR_9Z}: should expand (whole-string fallback to os.Getenv), got literal")
	}
}

func TestExpandWithEnv_EmptyString(t *testing.T) {
	got := expandWithEnv("", map[string]string{"FOO": "bar"})
	if got != "" {
		t.Errorf("empty string: got %q, want empty", got)
	}
}

func TestExpandWithEnv_NoVarRefs(t *testing.T) {
	got := expandWithEnv("plain string with no vars", map[string]string{"FOO": "bar"})
	if got != "plain string with no vars" {
		t.Errorf("plain string: got %q, want unchanged", got)
	}
}

func TestExpandWithEnv_MultipleVarRefs(t *testing.T) {
	// Two vars, both whole — both expand from env.
	env := map[string]string{"A": "alpha", "B": "beta"}
	got := expandWithEnv("$A and $B and more", env)
	if got != "alpha and beta and more" {
		t.Errorf("multiple vars: got %q, want alpha and beta and more", got)
	}
}

func TestExpandWithEnv_NumericVarRef(t *testing.T) {
	// $5 — starts with digit, not a valid identifier start.
	// Must return the literal "$5", not expand via os.Getenv.
	got := expandWithEnv("$5", map[string]string{"5": "five"})
	if got != "$5" {
		t.Errorf("$5: got %q, want literal $5", got)
	}
}

func TestExpandWithEnv_DollarEscape(t *testing.T) {
	// $$ → both $ written literally (each $ is not followed by an identifier char,
	// so it is written as-is). No special escape sequence for $$.
	got := expandWithEnv("$$", nil)
	if got != "$$" {
		t.Errorf("$$: got %q, want literal $$", got)
	}
}

func TestExpandWithEnv_MixedPartialAndWhole(t *testing.T) {
	// $A is in env (whole), $HOME is partial — only $A expands.
	env := map[string]string{"A": "alpha"}
	got := expandWithEnv("$A at $HOME", env)
	if got != "alpha at $HOME" {
		t.Errorf("$A at $HOME: got %q, want alpha at $HOME", got)
	}
}
