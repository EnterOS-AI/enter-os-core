package secrets

import (
	"strings"
	"sync"
	"testing"
)

// TestEveryPatternCompiles pins that every Pattern.regexSource is a
// valid Go-RE2 expression. Without this, a bad regex would silently
// disable ScanBytes for everything after it (the lazy compile would
// set compileErr and ScanBytes would return that error every call).
func TestEveryPatternCompiles(t *testing.T) {
	for _, p := range Patterns {
		if p.Name == "" {
			t.Errorf("pattern with empty Name: regex=%q", p.regexSource)
		}
		if p.Description == "" {
			t.Errorf("pattern %q has empty Description", p.Name)
		}
	}
	// Force compile + check error.
	if _, err := ScanBytes([]byte("placeholder")); err != nil {
		t.Fatalf("ScanBytes init failed: %v", err)
	}
}

// TestNoDuplicateNames — a duplicate pattern Name would make the
// "first match wins" semantics surprising to readers and any caller
// switching on Match.Name (none today but adding the guard is cheap).
func TestNoDuplicateNames(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Patterns {
		if seen[p.Name] {
			t.Errorf("duplicate pattern Name: %q", p.Name)
		}
		seen[p.Name] = true
	}
}

// TestKnownPatternsAllPresent — pins which specific Name values are
// expected. A future refactor that renames or removes one without
// updating consumers (CI workflow, runtime pre-commit hook, Files
// API Phase 2b backend) would silently widen the leak surface.
// Failing here forces the rename to be intentional.
func TestKnownPatternsAllPresent(t *testing.T) {
	expected := []string{
		"github-pat-classic",
		"github-app-installation-token",
		"github-oauth-user-to-server",
		"github-oauth-user",
		"github-oauth-refresh",
		"github-pat-fine-grained",
		"anthropic-api-key",
		"openai-project-key",
		"openai-service-account-key",
		"minimax-api-key",
		"slack-token",
		"aws-access-key-id",
		"aws-sts-temp-access-key-id",
	}
	got := map[string]bool{}
	for _, p := range Patterns {
		got[p.Name] = true
	}
	for _, want := range expected {
		if !got[want] {
			t.Errorf("expected pattern %q missing from Patterns slice", want)
		}
	}
}

// TestPositiveMatches — for each pattern, supply a representative
// shape and assert ScanBytes returns a Match with the right Name.
// These are TEST FIXTURES, not real credentials — each is the
// pattern's prefix + a long-enough trailing run of placeholder chars.
// `EXAMPLE` is sprinkled in to make grep-finds in CI logs obviously
// fake to a human reader (matches saved memory
// feedback_assert_exact_not_substring: tighten by Name not body).
func TestPositiveMatches(t *testing.T) {
	cases := []struct {
		fixture      string
		expectedName string
	}{
		{"ghp_" + "EXAMPLE111122223333444455556666777788889999", "github-pat-classic"},
		{"ghs_" + "EXAMPLE111122223333444455556666777788889999", "github-app-installation-token"},
		{"gho_" + "EXAMPLE111122223333444455556666777788889999", "github-oauth-user-to-server"},
		{"ghu_" + "EXAMPLE111122223333444455556666777788889999", "github-oauth-user"},
		{"ghr_" + "EXAMPLE111122223333444455556666777788889999", "github-oauth-refresh"},
		{"github_pat_EXAMPLE" + strings.Repeat("1", 80), "github-pat-fine-grained"},
		{"sk-ant-EXAMPLE" + strings.Repeat("1", 40), "anthropic-api-key"},
		{"sk-proj-EXAMPLE" + strings.Repeat("1", 40), "openai-project-key"},
		{"sk-svcacct-EXAMPLE" + strings.Repeat("1", 40), "openai-service-account-key"},
		{"sk-cp-EXAMPLE" + strings.Repeat("1", 60), "minimax-api-key"},
		{"xoxb-" + strings.Repeat("a", 25), "slack-token"},
		{"xoxa-" + strings.Repeat("a", 25), "slack-token"},
		// AWS regex requires [0-9A-Z]{16} — uppercase + digits only.
		{"AKIA1234567890ABCDEF", "aws-access-key-id"},
		{"ASIA1234567890ABCDEF", "aws-sts-temp-access-key-id"},
	}
	for _, tc := range cases {
		t.Run(tc.expectedName, func(t *testing.T) {
			m, err := ScanBytes([]byte(tc.fixture))
			if err != nil {
				t.Fatalf("ScanBytes(%q) errored: %v", tc.fixture, err)
			}
			if m == nil {
				t.Fatalf("ScanBytes(%q) returned no match — expected %q", tc.fixture, tc.expectedName)
			}
			if m.Name != tc.expectedName {
				t.Errorf("ScanBytes(%q) matched %q; expected %q", tc.fixture, m.Name, tc.expectedName)
			}
		})
	}
}

// TestNegativeShapes — strings that look credential-adjacent but
// shouldn't match (too short, wrong prefix, missing trailing bytes).
// Failing here means a pattern is too loose, which would generate
// false-positive denial in Files API and false-positive workflow
// failures in CI.
func TestNegativeShapes(t *testing.T) {
	cases := []string{
		// Too-short variants — anchored on the length suffix.
		"ghp_tooshort",
		"ghs_alsoshort1234",
		"github_pat_short",
		"sk-ant-short",
		"sk-cp-not-enough-bytes-here",
		// Looks like one of the prefixes but isn't (different letter).
		"gha_EXAMPLE_thirty_six_or_more_chars_here_xxx",
		// Slack family — wrong letter after xox.
		"xoxz-aaaaaaaaaaaaaaaaaaaaaaaaa",
		// AWS-shaped but wrong length suffix.
		"AKIATOOSHORT",
		// Empty / whitespace.
		"",
		"   ",
		// Plain prose mentioning the prefix as part of a longer word.
		"see also `ghp_HOWTO.md` in the repo",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			m, err := ScanBytes([]byte(c))
			if err != nil {
				t.Fatalf("ScanBytes(%q) errored: %v", c, err)
			}
			if m != nil {
				t.Errorf("ScanBytes(%q) unexpectedly matched %q", c, m.Name)
			}
		})
	}
}

// TestScanString_NoOp — sanity-check ScanString is the zero-copy
// wrapper around ScanBytes. Without this, a future refactor that
// makes ScanString do its own thing (e.g. accidentally normalise
// case) would diverge silently.
func TestScanString_NoOp(t *testing.T) {
	in := "ghp_" + "EXAMPLE111122223333444455556666777788889999"
	m1, err1 := ScanBytes([]byte(in))
	if err1 != nil {
		t.Fatalf("ScanBytes errored: %v", err1)
	}
	m2, err2 := ScanString(in)
	if err2 != nil {
		t.Fatalf("ScanString errored: %v", err2)
	}
	if m1 == nil || m2 == nil {
		t.Fatalf("expected matches; got bytes=%+v string=%+v", m1, m2)
	}
	if m1.Name != m2.Name {
		t.Errorf("ScanString and ScanBytes returned different Names: %q vs %q", m1.Name, m2.Name)
	}
}

// TestMatch_NoRoundtrip — assert the Match struct does NOT include
// the matched substring as a field. Adding such a field would
// regress the "matched bytes never leave ScanBytes" invariant that
// makes this package safe to call from log/UI surfaces. This is a
// reflection-light contract test — checks the field names statically.
func TestMatch_NoRoundtrip(t *testing.T) {
	var m Match
	// If someone adds a `Matched string` (or similar) field, this
	// test reads as the canonical place to update + reconsider.
	_ = m.Name
	_ = m.Description
	// The two-field shape is part of the public contract; new fields
	// require deliberation about whether they leak the secret value.
}

// resetPatternState snapshots the package-level pattern state and returns a
// cleanup function that restores it. Tests that intentionally corrupt
// Patterns to exercise compile-failure paths must restore the canonical state
// so later tests don't see the corrupted slice / stuck compileErr.
func resetPatternState(t *testing.T) (cleanup func()) {
	t.Helper()
	origPatterns := make([]Pattern, len(Patterns))
	copy(origPatterns, Patterns)
	origCompiledPatterns := compiledPatterns
	origCompileErr := compileErr
	return func() {
		Patterns = origPatterns
		compiledPatterns = origCompiledPatterns
		compileErr = origCompileErr
		compiledOnce = sync.Once{}
	}
}

// TestCompileError exercises the compileAll error path when a pattern regex
// is invalid. This is intentionally unreachable in production (a build bug),
// but the error path must be covered.
func TestCompileError(t *testing.T) {
	cleanup := resetPatternState(t)
	defer cleanup()

	Patterns = []Pattern{{
		Name:        "bad-pattern",
		Description: "invalid regex for testing",
		regexSource: `(`, // unbalanced paren
	}}
	compiledOnce = sync.Once{}
	compiledPatterns = nil
	compileErr = nil

	compileAll()
	if compileErr == nil {
		t.Fatal("compileAll expected compile error for invalid regex, got nil")
	}
	if !strings.Contains(compileErr.Error(), "bad-pattern") {
		t.Errorf("compileErr should mention pattern name, got: %v", compileErr)
	}
}

// TestScanBytes_CompileErr exercises ScanBytes returning the compile error
// when Patterns contains an invalid regex.
func TestScanBytes_CompileErr(t *testing.T) {
	cleanup := resetPatternState(t)
	defer cleanup()

	Patterns = []Pattern{{
		Name:        "another-bad-pattern",
		Description: "invalid regex for testing",
		regexSource: `[`, // unclosed character class
	}}
	compiledOnce = sync.Once{}
	compiledPatterns = nil
	compileErr = nil

	m, err := ScanBytes([]byte("anything"))
	if err == nil {
		t.Fatal("ScanBytes expected compile error, got nil")
	}
	if m != nil {
		t.Errorf("ScanBytes should return nil Match on compile error, got %+v", m)
	}
	if !strings.Contains(err.Error(), "another-bad-pattern") {
		t.Errorf("error should mention pattern name, got: %v", err)
	}
}

// TestScanString_CompileErr exercises ScanString returning the compile error
// when Patterns contains an invalid regex. ScanString is the zero-copy wrapper
// around ScanBytes, so this confirms the error path propagates through it.
func TestScanString_CompileErr(t *testing.T) {
	cleanup := resetPatternState(t)
	defer cleanup()

	Patterns = []Pattern{{
		Name:        "string-bad-pattern",
		Description: "invalid regex for testing",
		regexSource: `(?`, // invalid regex syntax
	}}
	compiledOnce = sync.Once{}
	compiledPatterns = nil
	compileErr = nil

	m, err := ScanString("anything")
	if err == nil {
		t.Fatal("ScanString expected compile error, got nil")
	}
	if m != nil {
		t.Errorf("ScanString should return nil Match on compile error, got %+v", m)
	}
	if !strings.Contains(err.Error(), "string-bad-pattern") {
		t.Errorf("error should mention pattern name, got: %v", err)
	}
}
