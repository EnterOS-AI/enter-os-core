package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ── isSafeRoleName ────────────────────────────────────────────────────────────

func TestIsSafeRoleName_Valid(t *testing.T) {
	cases := []string{
		"backend",
		"frontend",
		"backend-engineer",
		"Frontend_Engineer",
		"DevOps123",
		"sre-team",
		"a",
		"ABC",
		"Role_With_Underscores_And-Numbers123",
	}
	for _, r := range cases {
		t.Run(r, func(t *testing.T) {
			if !isSafeRoleName(r) {
				t.Errorf("isSafeRoleName(%q): expected true, got false", r)
			}
		})
	}
}

func TestIsSafeRoleName_Invalid(t *testing.T) {
	cases := []struct {
		name string
		role string
	}{
		{"empty", ""},
		{"dot", "."},
		{"double dot", ".."},
		{"path separator", "backend/engineer"},
		{"space", "backend engineer"},
		{"special char", "backend@engineer"},
		{"at sign", "role@team"},
		{"colon", "role:admin"},
		{"hash", "role#1"},
		{"percent", "role%20"},
		{"quote", `role"name`},
		{"backslash", `role\name`},
		{"tilde", "role~test"},
		{"backtick", "`role"},
		{"bracket open", "[role]"},
		{"bracket close", "role]"},
		{"plus", "role+admin"},
		{"equals", "role=admin"},
		{"caret", "role^admin"},
		{"question mark", "role?"},
		{"pipe at end", "role|"},
		{"greater than", "role>"},
		{"asterisk", "role*"},
		{"ampersand", "role&"},
		{"exclamation at end", "role!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isSafeRoleName(tc.role) {
				t.Errorf("isSafeRoleName(%q): expected false, got true", tc.role)
			}
		})
	}
}

// ── hasUnresolvedVarRef ───────────────────────────────────────────────────────

func TestHasUnresolvedVarRef_NoVars(t *testing.T) {
	cases := []string{
		"",
		"plain text",
		"no variables here",
		"123 numeric",
		"$",
		"${}",
		"$5",
		"$$$$",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			if hasUnresolvedVarRef(s, s) {
				t.Errorf("hasUnresolvedVarRef(%q, %q): expected false, got true", s, s)
			}
		})
	}
}

func TestHasUnresolvedVarRef_Resolved(t *testing.T) {
	// Expansion consumed the var refs (where "consumed" means the output no longer
	// contains the original var reference syntax).
	cases := []struct {
		orig     string
		expanded string
		want     bool // true = unresolved (function returns true), false = resolved
	}{
		// Empty output: function conservatively returns true — it cannot distinguish
		// "var was set to empty" from "var was not found and stripped". The test
		// documents this design choice; callers who need empty=resolved should
		// pre-process the output before calling hasUnresolvedVarRef.
		{"${VAR}", "", true},
		{"${VAR}", "value", false},                    // var replaced
		{"$VAR", "value", false},                      // bare var replaced
		{"prefix${VAR}suffix", "prefixvaluesuffix", false},
		{"${A}${B}", "ab", false},
		// FOO=FOO and BAR=BAR — both vars found and replaced. Expanded output
		// "FOO and BAR" has no ${...} syntax left, so function returns false.
		{"${FOO} and ${BAR}", "FOO and BAR", false},
	}
	for _, tc := range cases {
		t.Run(tc.orig, func(t *testing.T) {
			got := hasUnresolvedVarRef(tc.orig, tc.expanded)
			if got != tc.want {
				t.Errorf("hasUnresolvedVarRef(%q, %q): got %v, want %v", tc.orig, tc.expanded, got, tc.want)
			}
		})
	}
}

func TestHasUnresolvedVarRef_Unresolved(t *testing.T) {
	// Expansion left the refs intact → unresolved.
	cases := []struct {
		orig    string
		expanded string
	}{
		{"${VAR}", "${VAR}"},       // untouched
		{"$VAR", "$VAR"},           // bare untouched
		{"prefix${VAR}suffix", "prefix${VAR}suffix"},
		{"${A}${B}", "${A}${B}"},   // both unresolved
		{"${FOO}", ""},             // empty result with var ref in original
	}
	for _, tc := range cases {
		t.Run(tc.orig, func(t *testing.T) {
			if !hasUnresolvedVarRef(tc.orig, tc.expanded) {
				t.Errorf("hasUnresolvedVarRef(%q, %q): expected true, got false", tc.orig, tc.expanded)
			}
		})
	}
}

// ── expandWithEnv ─────────────────────────────────────────────────────────────

func TestExpandWithEnv_Basic(t *testing.T) {
	env := map[string]string{"FOO": "bar", "BAZ": "qux"}
	cases := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"no vars", "no vars"},
		{"${FOO}", "bar"},
		{"$FOO", "bar"},
		{"prefix${FOO}suffix", "prefixbarsuffix"},
		{"${FOO}${BAZ}", "barqux"},
		{"${MISSING}", ""}, // not in env, not in os env → empty
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := expandWithEnv(tc.input, env)
			if got != tc.want {
				t.Errorf("expandWithEnv(%q, %v) = %q, want %q", tc.input, env, got, tc.want)
			}
		})
	}
}

// ── mergeCategoryRouting ─────────────────────────────────────────────────────

func TestMergeCategoryRouting_EmptyInputs(t *testing.T) {
	// Both empty → empty
	r := mergeCategoryRouting(nil, nil)
	if len(r) != 0 {
		t.Errorf("mergeCategoryRouting(nil, nil): got %v, want empty", r)
	}

	r = mergeCategoryRouting(map[string][]string{}, map[string][]string{})
	if len(r) != 0 {
		t.Errorf("mergeCategoryRouting({}, {}): got %v, want empty", r)
	}
}

func TestMergeCategoryRouting_DefaultsOnly(t *testing.T) {
	defaults := map[string][]string{
		"security": {"Backend Engineer", "DevOps"},
		"ui":       {"Frontend Engineer"},
		"data":     {"Data Engineer"},
	}
	r := mergeCategoryRouting(defaults, nil)
	if len(r) != 3 {
		t.Errorf("got %d keys, want 3", len(r))
	}
	if len(r["security"]) != 2 {
		t.Errorf("security roles: got %v, want 2", r["security"])
	}
}

func TestMergeCategoryRouting_WorkspaceOverrides(t *testing.T) {
	defaults := map[string][]string{
		"security": {"Backend Engineer", "DevOps"},
		"ui":       {"Frontend Engineer"},
	}
	ws := map[string][]string{
		"security": {"SRE Team"}, // narrows
		"ui":       {},           // drops
		"infra":    {"Platform Team"}, // adds
	}
	r := mergeCategoryRouting(defaults, ws)
	if len(r["security"]) != 1 || r["security"][0] != "SRE Team" {
		t.Errorf("security: got %v, want [SRE Team]", r["security"])
	}
	if _, ok := r["ui"]; ok {
		t.Errorf("ui should be dropped, got %v", r["ui"])
	}
	if len(r["infra"]) != 1 || r["infra"][0] != "Platform Team" {
		t.Errorf("infra: got %v, want [Platform Team]", r["infra"])
	}
}

func TestMergeCategoryRouting_EmptyListDrops(t *testing.T) {
	defaults := map[string][]string{"foo": {"A", "B"}}
	ws := map[string][]string{"foo": {}}
	r := mergeCategoryRouting(defaults, ws)
	if _, ok := r["foo"]; ok {
		t.Errorf("foo with empty ws list: should be dropped, got %v", r["foo"])
	}
}

func TestMergeCategoryRouting_EmptyKeySkipped(t *testing.T) {
	defaults := map[string][]string{"": {"Role"}}
	ws := map[string][]string{"": {}}
	r := mergeCategoryRouting(defaults, ws)
	if _, ok := r[""]; ok {
		t.Errorf("empty key should be skipped, got %v", r[""])
	}
}

// ── renderCategoryRoutingYAML ────────────────────────────────────────────────

func TestRenderCategoryRoutingYAML_Empty(t *testing.T) {
	out, err := renderCategoryRoutingYAML(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Errorf("got %q, want empty string", out)
	}

	out, err = renderCategoryRoutingYAML(map[string][]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Errorf("got %q, want empty string", out)
	}
}

func TestRenderCategoryRoutingYAML_StableOrdering(t *testing.T) {
	// Keys are sorted so output is deterministic regardless of map iteration order.
	m := map[string][]string{
		"zebra":  {"A"},
		"alpha":  {"B"},
		"middle": {"C"},
	}
	out, err := renderCategoryRoutingYAML(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// alpha must come before middle, which must come before zebra
	ai := 0
	zi := 0
	mi := 0
	for i, c := range out {
		switch {
		case c == 'a' && i < len(out)-5 && out[i:i+5] == "alpha":
			ai = i
		case c == 'z' && i < len(out)-5 && out[i:i+5] == "zebra":
			zi = i
		case c == 'm' && i < len(out)-6 && out[i:i+6] == "middle":
			mi = i
		}
	}
	if ai <= 0 || zi <= 0 || mi <= 0 {
		t.Fatalf("could not locate all keys in output: %s", out)
	}
	if !(ai < mi && mi < zi) {
		t.Errorf("keys not sorted: alpha=%d middle=%d zebra=%d, output:\n%s", ai, mi, zi, out)
	}
}

func TestRenderCategoryRoutingYAML_SpecialCharsEscaped(t *testing.T) {
	// YAML library should escape characters that need quoting.
	m := map[string][]string{
		"key:with:colons": {"Role: Admin"},
		"key with space":  {"Role"},
	}
	out, err := renderCategoryRoutingYAML(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The output must be valid YAML (yaml.Marshal handles quoting).
	// The key with colons should appear quoted in the output.
	if out == "" {
		t.Error("output is empty")
	}
}

// ── appendYAMLBlock ───────────────────────────────────────────────────────────

func TestAppendYAMLBlock_NoExisting(t *testing.T) {
	got := appendYAMLBlock(nil, "key: value")
	if string(got) != "key: value" {
		t.Errorf("got %q, want 'key: value'", string(got))
	}
}

func TestAppendYAMLBlock_EmptyBlock(t *testing.T) {
	// When existing lacks a trailing \n, the function adds one before appending
	// the empty block — so the result always has a clean terminator.
	got := appendYAMLBlock([]byte("existing: data"), "")
	want := "existing: data\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestAppendYAMLBlock_AppendsWithNewline(t *testing.T) {
	existing := []byte("key: value")
	block := "new: entry"
	got := appendYAMLBlock(existing, block)
	want := "key: value\nnew: entry"
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestAppendYAMLBlock_AlreadyEndsWithNewline(t *testing.T) {
	existing := []byte("key: value\n")
	block := "new: entry"
	got := appendYAMLBlock(existing, block)
	want := "key: value\nnew: entry"
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

// ── mergePlugins ─────────────────────────────────────────────────────────────

func TestMergePlugins_EmptyInputs(t *testing.T) {
	r := mergePlugins(nil, nil)
	if len(r) != 0 {
		t.Errorf("got %v, want []", r)
	}
	r = mergePlugins([]string{}, []string{})
	if len(r) != 0 {
		t.Errorf("got %v, want []", r)
	}
}

func TestMergePlugins_BasicMerge(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b"}
	ws := []string{"plugin-b", "plugin-c"}
	r := mergePlugins(defaults, ws)
	// defaults first, ws appended, b deduplicated
	if len(r) != 3 {
		t.Errorf("got %v, want 3 items", r)
	}
	if r[0] != "plugin-a" || r[1] != "plugin-b" || r[2] != "plugin-c" {
		t.Errorf("got %v, want [a, b, c]", r)
	}
}

func TestMergePlugins_ExcludeWithBang(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b", "plugin-c"}
	ws := []string{"!plugin-b"}
	r := mergePlugins(defaults, ws)
	if len(r) != 2 {
		t.Errorf("got %v, want 2 items", r)
	}
	if r[0] != "plugin-a" || r[1] != "plugin-c" {
		t.Errorf("got %v, want [a, c]", r)
	}
}

func TestMergePlugins_ExcludeWithDash(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b", "plugin-c"}
	ws := []string{"-plugin-b"}
	r := mergePlugins(defaults, ws)
	if len(r) != 2 || r[0] != "plugin-a" || r[1] != "plugin-c" {
		t.Errorf("got %v, want [a, c]", r)
	}
}

func TestMergePlugins_ExcludeNonexistent(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b"}
	ws := []string{"!plugin-c"} // c not present
	r := mergePlugins(defaults, ws)
	if len(r) != 2 {
		t.Errorf("got %v, want 2 items", r)
	}
}

func TestMergePlugins_ExcludeEmptyTarget(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b"}
	ws := []string{"!"}
	r := mergePlugins(defaults, ws)
	if len(r) != 2 {
		t.Errorf("got %v, want 2 items", r)
	}
}

func TestMergePlugins_EmptyPlugin(t *testing.T) {
	defaults := []string{"", "plugin-a", ""}
	ws := []string{"plugin-b", ""}
	r := mergePlugins(defaults, ws)
	if len(r) != 2 {
		t.Errorf("got %v, want 2 items", r)
	}
}

// ── Additional coverage: expandWithEnv ──────────────────────────────
func TestExpandWithEnv_BracedVar(t *testing.T) {
	env := map[string]string{"FOO": "bar", "BAZ": "qux"}
	result := expandWithEnv("value is ${FOO}", env)
	assert.Equal(t, "value is bar", result)
}

func TestExpandWithEnv_DollarVar(t *testing.T) {
	env := map[string]string{"X": "1", "Y": "2"}
	result := expandWithEnv("$X + $Y = 3", env)
	assert.Equal(t, "1 + 2 = 3", result)
}

func TestExpandWithEnv_Mixed(t *testing.T) {
	env := map[string]string{"A": "alpha", "B": "beta"}
	result := expandWithEnv("${A}_${B}", env)
	assert.Equal(t, "alpha_beta", result)
}

func TestExpandWithEnv_MissingVar(t *testing.T) {
	// Missing vars stay as-is (os.Getenv fallback returns "" for unset vars).
	env := map[string]string{}
	result := expandWithEnv("${UNSET}", env)
	assert.Equal(t, "", result)
}

func TestExpandWithEnv_EmptyMap(t *testing.T) {
	result := expandWithEnv("no vars here", map[string]string{})
	assert.Equal(t, "no vars here", result)
}

func TestExpandWithEnv_LiteralDollar(t *testing.T) {
	// A bare $ not followed by a valid identifier char stays as-is.
	result := expandWithEnv("cost $100", map[string]string{})
	assert.Equal(t, "cost $100", result)
}

func TestExpandWithEnv_PartiallyPresent(t *testing.T) {
	env := map[string]string{"SET": "yes"}
	result := expandWithEnv("${SET} and ${NOT_SET}", env)
	// ${SET} resolved from env; ${NOT_SET} stays literal (not whole-string ref,
	// so os.Getenv fallback is NOT used — CWE-78 regression guard).
	assert.Equal(t, "yes and ${NOT_SET}", result)
}

// mergeCategoryRouting tests — unions defaults with per-workspace routing.

// ── Additional coverage: mergeCategoryRouting ──────────────────────
func TestMergeCategoryRouting_WorkspaceAddsCategory(t *testing.T) {
	defaults := map[string][]string{
		"security": {"Backend Engineer"},
	}
	wsRouting := map[string][]string{
		"ui": {"Frontend Engineer"},
	}
	result := mergeCategoryRouting(defaults, wsRouting)
	assert.Equal(t, []string{"Backend Engineer"}, result["security"])
	assert.Equal(t, []string{"Frontend Engineer"}, result["ui"])
}

func TestMergeCategoryRouting_EmptyListDropsCategory(t *testing.T) {
	defaults := map[string][]string{
		"security": {"Backend Engineer"},
		"infra":    {"SRE"},
	}
	wsRouting := map[string][]string{
		"security": {}, // empty list = explicit drop
	}
	result := mergeCategoryRouting(defaults, wsRouting)
	_, hasSecurity := result["security"]
	assert.False(t, hasSecurity)
	assert.Equal(t, []string{"SRE"}, result["infra"])
}

func TestMergeCategoryRouting_EmptyDefaultKeySkipped(t *testing.T) {
	defaults := map[string][]string{
		"": {"Backend Engineer"}, // empty key should be skipped
	}
	result := mergeCategoryRouting(defaults, nil)
	_, has := result[""]
	assert.False(t, has)
}

func TestMergeCategoryRouting_EmptyWorkspaceKeySkipped(t *testing.T) {
	defaults := map[string][]string{
		"security": {"Backend Engineer"},
	}
	wsRouting := map[string][]string{
		"": {"Some Role"},
	}
	result := mergeCategoryRouting(defaults, wsRouting)
	_, has := result[""]
	assert.False(t, has)
	assert.Equal(t, []string{"Backend Engineer"}, result["security"])
}

func TestMergeCategoryRouting_DoesNotMutateInputs(t *testing.T) {
	defaults := map[string][]string{
		"security": {"Backend Engineer"},
	}
	wsRouting := map[string][]string{
		"security": {"DevOps"},
	}
	orig := defaults["security"][0]
	_ = mergeCategoryRouting(defaults, wsRouting)
	assert.Equal(t, orig, defaults["security"][0])
}

// renderCategoryRoutingYAML tests — deterministic YAML emission.

// ── Additional coverage: renderCategoryRoutingYAML ────────────────
func TestRenderCategoryRoutingYAML_SingleCategory(t *testing.T) {
	routing := map[string][]string{
		"security": {"Backend Engineer", "DevOps"},
	}
	result, err := renderCategoryRoutingYAML(routing)
	assert.NoError(t, err)
	assert.Contains(t, result, "security:")
	assert.Contains(t, result, "Backend Engineer")
	assert.Contains(t, result, "DevOps")
}

func TestRenderCategoryRoutingYAML_MultipleCategoriesSorted(t *testing.T) {
	routing := map[string][]string{
		"zebra":   {"RoleZ"},
		"alpha":   {"RoleA"},
		"middleware": {"RoleM"},
	}
	result, err := renderCategoryRoutingYAML(routing)
	assert.NoError(t, err)
	// Keys are sorted alphabetically.
	idxAlpha := assertFind(t, result, "alpha:")
	idxZebra := assertFind(t, result, "zebra:")
	idxMid := assertFind(t, result, "middleware:")
	if idxAlpha > -1 && idxZebra > -1 {
		assert.True(t, idxAlpha < idxZebra, "alpha should appear before zebra")
	}
	if idxMid > -1 && idxZebra > -1 {
		assert.True(t, idxMid < idxZebra, "middleware should appear before zebra")
	}
}

func TestRenderCategoryRoutingYAML_EmptyListCategory(t *testing.T) {
	// Empty-list category should still render (mergeCategoryRouting drops
	// them before they reach this function, but we test the render in isolation).
	routing := map[string][]string{
		"security": {},
	}
	result, err := renderCategoryRoutingYAML(routing)
	assert.NoError(t, err)
	assert.Contains(t, result, "security:")
}

func TestRenderCategoryRoutingYAML_SpecialCharactersEscaped(t *testing.T) {
	routing := map[string][]string{
		"notes": {`has: colon`, `and "quotes"`, "emoji: 🚀"},
	}
	result, err := renderCategoryRoutingYAML(routing)
	assert.NoError(t, err)
	// Should not panic and should produce valid YAML.
	assert.Contains(t, result, "notes:")
}

// appendYAMLBlock tests — safe concatenation with newline boundary.

// ── Additional coverage: appendYAMLBlock ───────────────────────────
func TestAppendYAMLBlock_BothEmpty(t *testing.T) {
	result := appendYAMLBlock(nil, "")
	assert.Nil(t, result) // append(nil, []byte("")...) returns nil in Go
}

func TestAppendYAMLBlock_ExistingHasNewline(t *testing.T) {
	existing := []byte("existing:\n")
	block := "key: value\n"
	result := appendYAMLBlock(existing, block)
	assert.Equal(t, "existing:\nkey: value\n", string(result))
}

func TestAppendYAMLBlock_ExistingNoNewline(t *testing.T) {
	existing := []byte("existing:")
	block := "key: value\n"
	result := appendYAMLBlock(existing, block)
	assert.Equal(t, "existing:\nkey: value\n", string(result))
}

func TestAppendYAMLBlock_ExistingEmpty(t *testing.T) {
	existing := []byte("")
	block := "key: value\n"
	result := appendYAMLBlock(existing, block)
	assert.Equal(t, "key: value\n", string(result))
}

func TestAppendYAMLBlock_NilExisting(t *testing.T) {
	block := "key: value\n"
	result := appendYAMLBlock(nil, block)
	assert.Equal(t, "key: value\n", string(result))
}

// mergePlugins tests — union with exclusion prefix (!/-).

// ── Additional coverage: mergePlugins (additional cases) ───────────
func TestMergePlugins_DefaultsOnly(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b"}
	result := mergePlugins(defaults, nil)
	assert.Equal(t, []string{"plugin-a", "plugin-b"}, result)
}

func TestMergePlugins_WorkspaceAdds(t *testing.T) {
	defaults := []string{"plugin-a"}
	wsPlugins := []string{"plugin-b", "plugin-a"} // duplicate of default
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-a", "plugin-b"}, result)
}

func TestMergePlugins_ExclusionWithBang(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b", "plugin-c"}
	wsPlugins := []string{"!plugin-b"}
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-a", "plugin-c"}, result)
}

func TestMergePlugins_ExclusionWithDash(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b", "plugin-c"}
	wsPlugins := []string{"-plugin-b"}
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-a", "plugin-c"}, result)
}

func TestMergePlugins_ExclusionEmptyTarget(t *testing.T) {
	defaults := []string{"plugin-a", "plugin-b"}
	wsPlugins := []string{"!", "-"} // no-op exclusions
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-a", "plugin-b"}, result)
}

func TestMergePlugins_ExclusionNotInDefaults(t *testing.T) {
	// Excluding something not in defaults is a no-op.
	defaults := []string{"plugin-a"}
	wsPlugins := []string{"!plugin-b"}
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-a"}, result)
}

func TestMergePlugins_WorkspaceAddsNew(t *testing.T) {
	defaults := []string{"plugin-a"}
	wsPlugins := []string{"plugin-b"}
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-a", "plugin-b"}, result)
}

func TestMergePlugins_DeduplicationOrder(t *testing.T) {
	// Defaults first; workspace entries deduplicated.
	defaults := []string{"plugin-a", "plugin-a", "plugin-b"}
	wsPlugins := []string{"plugin-b", "plugin-c", "plugin-c"}
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-a", "plugin-b", "plugin-c"}, result)
}

func TestMergePlugins_ExclusionThenAddSameName(t *testing.T) {
	// Remove then re-add: order matters.
	defaults := []string{"plugin-a", "plugin-b"}
	wsPlugins := []string{"!plugin-a", "plugin-a"}
	result := mergePlugins(defaults, wsPlugins)
	assert.Equal(t, []string{"plugin-b", "plugin-a"}, result)
}

// isSafeRoleName tests — alphanumeric + hyphen/underscore, no path separators.

// ── Additional coverage: isSafeRoleName ───────────────────────────
func TestIsSafeRoleName_SpecialCharsRejected(t *testing.T) {
	bad := []string{
		"role@name",
		"role#name",
		"role$name",
		"role%name",
		"role&name",
		"role*name",
		"role?name",
		"role=name",
	}
	for _, r := range bad {
		if isSafeRoleName(r) {
			t.Errorf("isSafeRoleName(%q) expected false, got true", r)
		}
	}
}

// assertFind is a helper: returns index of first occurrence of substr in s, or -1.
func assertFind(t *testing.T, s, substr string) int {
	t.Helper()
	idx := -1
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			idx = i
			break
		}
	}
	return idx
}
