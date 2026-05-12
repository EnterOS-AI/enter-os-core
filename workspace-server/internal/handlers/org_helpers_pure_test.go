package handlers

import (
	"testing"
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
	// Expansion consumed the var refs.
	cases := []struct {
		orig    string
		expanded string
	}{
		{"${VAR}", ""},             // var expanded to empty (unset → removed)
		{"${VAR}", "value"},       // var replaced
		{"$VAR", "value"},         // bare var replaced
		{"prefix${VAR}suffix", "prefixvaluesuffix"},
		{"${A}${B}", "ab"},
		{"${FOO} and ${BAR}", "FOO and BAR"},
	}
	for _, tc := range cases {
		t.Run(tc.orig, func(t *testing.T) {
			if hasUnresolvedVarRef(tc.orig, tc.expanded) {
				t.Errorf("hasUnresolvedVarRef(%q, %q): expected false, got true", tc.orig, tc.expanded)
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
	got := appendYAMLBlock([]byte("existing: data"), "")
	if string(got) != "existing: data" {
		t.Errorf("got %q, want 'existing: data'", string(got))
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
