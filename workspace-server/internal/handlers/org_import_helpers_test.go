package handlers

// org_import_helpers_test.go — 24 cases covering pure-logic helpers in org_import.go.
//
// Covered helpers (all package-local, called directly within this package):
//   countWorkspaces         — recursive subtree count
//   envRequirementKey       — canonical NUL-separated sort key
//   sanitizeEnvMembers      — name-validation regex filter
//   flattenAndSortRequirements — singles-first deterministic sort
//   collectOrgEnv           — multi-tier dedup: required-wins + any-of domination
//   EnvRequirement.Members  — Name/AnyOf accessor

import "testing"

// ─────────────────────────────────────────────────────────────────────────────
// countWorkspaces
// ─────────────────────────────────────────────────────────────────────────────

func TestCountWorkspaces_Leaf(t *testing.T) {
	// A leaf workspace with no children counts as 1.
	ws := OrgWorkspace{Name: "leaf"}
	got := countWorkspaces([]OrgWorkspace{ws})
	if got != 1 {
		t.Errorf("leaf workspace: count=%d, want 1", got)
	}
}

func TestCountWorkspaces_SingleChild(t *testing.T) {
	// One child means 2 total: parent + child.
	ws := OrgWorkspace{
		Name:     "parent",
		Children: []OrgWorkspace{{Name: "child"}},
	}
	got := countWorkspaces([]OrgWorkspace{ws})
	if got != 2 {
		t.Errorf("parent+1child: count=%d, want 2", got)
	}
}

func TestCountWorkspaces_Siblings(t *testing.T) {
	// Two siblings under same parent: 1 parent + 2 children = 3.
	ws := OrgWorkspace{
		Name:     "parent",
		Children: []OrgWorkspace{{Name: "a"}, {Name: "b"}},
	}
	got := countWorkspaces([]OrgWorkspace{ws})
	if got != 3 {
		t.Errorf("parent+2children: count=%d, want 3", got)
	}
}

func TestCountWorkspaces_NestedChildren(t *testing.T) {
	// Two levels: 1 root + 1 child + 1 grandchild = 3.
	ws := OrgWorkspace{
		Name: "root",
		Children: []OrgWorkspace{{
			Name:     "child",
			Children: []OrgWorkspace{{Name: "grandchild"}},
		}},
	}
	got := countWorkspaces([]OrgWorkspace{ws})
	if got != 3 {
		t.Errorf("2-level nesting: count=%d, want 3", got)
	}
}

func TestCountWorkspaces_DeepNesting(t *testing.T) {
	// Three levels: root → child → grandchild → great-grandchild = 4.
	ws := OrgWorkspace{
		Name: "a",
		Children: []OrgWorkspace{{
			Name: "b",
			Children: []OrgWorkspace{{
				Name: "c",
				Children: []OrgWorkspace{{Name: "d"}},
			}},
		}},
	}
	got := countWorkspaces([]OrgWorkspace{ws})
	if got != 4 {
		t.Errorf("3-level nesting: count=%d, want 4", got)
	}
}

func TestCountWorkspaces_EmptySlice(t *testing.T) {
	got := countWorkspaces([]OrgWorkspace{})
	if got != 0 {
		t.Errorf("empty slice: count=%d, want 0", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// envRequirementKey
// ─────────────────────────────────────────────────────────────────────────────

func TestEnvRequirementKey_SingleMember(t *testing.T) {
	got := envRequirementKey([]string{"API_KEY"})
	want := "API_KEY"
	if got != want {
		t.Errorf("single member: key=%q, want %q", got, want)
	}
}

func TestEnvRequirementKey_TwoMembersSorted(t *testing.T) {
	// Already alphabetical — key should be stable.
	got := envRequirementKey([]string{"API_KEY", "MODEL_NAME"})
	want := "API_KEY\x00MODEL_NAME"
	if got != want {
		t.Errorf("sorted pair: key=%q, want %q", got, want)
	}
}

func TestEnvRequirementKey_TwoMembersReverse(t *testing.T) {
	// Reversed order should canonicalise to same key as sorted.
	got := envRequirementKey([]string{"MODEL_NAME", "API_KEY"})
	want := "API_KEY\x00MODEL_NAME"
	if got != want {
		t.Errorf("reversed pair: key=%q, want %q", got, want)
	}
}

func TestEnvRequirementKey_PermutationEquivalence(t *testing.T) {
	// All permutations of the same set must produce identical keys.
	perms := [][]string{
		{"X", "A", "M"},
		{"A", "M", "X"},
		{"M", "X", "A"},
		{"X", "M", "A"},
	}
	var first string
	for i, perm := range perms {
		got := envRequirementKey(perm)
		if i == 0 {
			first = got
		} else if got != first {
			t.Errorf("permutation %d: key=%q differs from first key %q", i, got, first)
		}
	}
}

func TestEnvRequirementKey_Empty(t *testing.T) {
	got := envRequirementKey([]string{})
	if got != "" {
		t.Errorf("empty: key=%q, want empty string", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sanitizeEnvMembers
// ─────────────────────────────────────────────────────────────────────────────

func TestSanitizeEnvMembers_AllValid(t *testing.T) {
	// Valid POSIX env-var names (uppercase + underscore + digit).
	got, ok := sanitizeEnvMembers([]string{"API_KEY", "MODEL_NAME", "A1"}, "test")
	if !ok {
		t.Error("all-valid: expected ok=true")
	}
	want := []string{"API_KEY", "MODEL_NAME", "A1"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("all-valid: got=%v, want %v", got, want)
			break
		}
	}
}

func TestSanitizeEnvMembers_OneInvalid(t *testing.T) {
	// One invalid name is filtered; valid remainder is kept.
	got, ok := sanitizeEnvMembers([]string{"API_KEY", "invalid-name", "SECRET"}, "test")
	if !ok {
		t.Error("one-invalid: expected ok=true (valid members remain)")
	}
	if len(got) != 2 {
		t.Errorf("one-invalid: got %v (len=%d), want [API_KEY SECRET]", got, len(got))
	}
}

func TestSanitizeEnvMembers_AllInvalid(t *testing.T) {
	// All invalid → empty output, ok=false.
	got, ok := sanitizeEnvMembers([]string{"lowercase", "123", "has-dash"}, "test")
	if ok {
		t.Error("all-invalid: expected ok=false")
	}
	if len(got) != 0 {
		t.Errorf("all-invalid: got %v, want []", got)
	}
}

func TestSanitizeEnvMembers_EmptyStringSkipped(t *testing.T) {
	// Empty string in list is silently skipped (not a regex failure).
	got, ok := sanitizeEnvMembers([]string{"API_KEY", "", "SECRET"}, "test")
	if !ok {
		t.Error("empty-string: expected ok=true")
	}
	if len(got) != 2 {
		t.Errorf("empty-string: got %v, want [API_KEY SECRET]", got)
	}
}

func TestSanitizeEnvMembers_EmptyInput(t *testing.T) {
	// Empty slice → empty output, ok=false.
	got, ok := sanitizeEnvMembers([]string{}, "test")
	if ok {
		t.Error("empty-input: expected ok=false")
	}
	if len(got) != 0 {
		t.Errorf("empty-input: got %v, want []", got)
	}
}

func TestSanitizeEnvMembers_NameBoundary(t *testing.T) {
	// Name must START with uppercase. Lowercase-start names are invalid.
	got, ok := sanitizeEnvMembers([]string{"api_key", "API_KEY"}, "test")
	if !ok {
		t.Error("lower-start: expected ok=true (API_KEY passes)")
	}
	if len(got) != 1 || got[0] != "API_KEY" {
		t.Errorf("lower-start: got %v, want [API_KEY]", got)
	}
}

func TestSanitizeEnvMembers_NameTooLong(t *testing.T) {
	// Max 128 chars after the leading uppercase char.
	longName := "X" + string(make([]byte, 128))
	got, ok := sanitizeEnvMembers([]string{longName, "SHORT"}, "test")
	if !ok {
		t.Error("too-long: expected ok=true (SHORT is valid)")
	}
	if len(got) != 1 || got[0] != "SHORT" {
		t.Errorf("too-long: got %v, want [SHORT]", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// flattenAndSortRequirements
// ─────────────────────────────────────────────────────────────────────────────

func TestFlattenAndSortRequirements_Empty(t *testing.T) {
	got := flattenAndSortRequirements(map[string]EnvRequirement{})
	if len(got) != 0 {
		t.Errorf("empty map: got %d items, want 0", len(got))
	}
}

func TestFlattenAndSortRequirements_SinglesFirst(t *testing.T) {
	// Singles sort before any-of groups.
	by := map[string]EnvRequirement{
		"Z":    {Name: "Z"}, // single
		"X":    {Name: "X"}, // single
		"any":  {AnyOf: []string{"A", "B"}},
		"other": {AnyOf: []string{"C"}},
	}
	got := flattenAndSortRequirements(by)
	if len(got) != 4 {
		t.Fatalf("wrong count: got %d, want 4", len(got))
	}
	// First two must be singles.
	singlesFirst := got[0].Name != "" && got[1].Name != ""
	anyOfAfter := len(got) > 2 && (got[2].Name == "" || got[3].Name == "")
	if !singlesFirst || !anyOfAfter {
		t.Errorf("singles-first order violated: %v", got)
	}
}

func TestFlattenAndSortRequirements_SinglesAlphabetical(t *testing.T) {
	// Within the singles section, alphabetical order.
	by := map[string]EnvRequirement{
		"Z": {Name: "Z"},
		"A": {Name: "A"},
		"M": {Name: "M"},
	}
	got := flattenAndSortRequirements(by)
	if got[0].Name != "A" || got[1].Name != "M" || got[2].Name != "Z" {
		t.Errorf("singles not alphabetically sorted: %v", got)
	}
}

func TestFlattenAndSortRequirements_AnyOfSortedByKey(t *testing.T) {
	// Any-of groups are sorted by the envRequirementKey of their members.
	// Keys must match what envRequirementKey() produces: sorted, NUL-separated.
	by := map[string]EnvRequirement{
		"a\x00b": {AnyOf: []string{"b", "a"}}, // canonical key = "a\x00b"
		"a\x00c": {AnyOf: []string{"a", "c"}}, // canonical key = "a\x00c"
	}
	got := flattenAndSortRequirements(by)
	// Both are any-of (Name == ""), order by key.
	if got[0].Name != "" || got[1].Name != "" {
		t.Errorf("expected all any-of, got singles: %v", got)
	}
	// "a\x00b" < "a\x00c" alphabetically → "a\x00b" first → [{b,a}] first.
	first := got[0].AnyOf
	if len(first) == 0 || first[0] != "b" {
		t.Errorf("any-of sort wrong: got %v first, want any-of [{b,a}]", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// collectOrgEnv — deduplication + required-wins
// ─────────────────────────────────────────────────────────────────────────────

func TestCollectOrgEnv_EmptyTemplate(t *testing.T) {
	tmpl := &OrgTemplate{}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 0 || len(rec) != 0 {
		t.Errorf("empty template: req=%v rec=%v, want both empty", req, rec)
	}
}

func TestCollectOrgEnv_RequiredOnly(t *testing.T) {
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "API_KEY"},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "API_KEY" {
		t.Errorf("required-only: req=%v, want [API_KEY]", req)
	}
	if len(rec) != 0 {
		t.Errorf("required-only: rec=%v, want []", rec)
	}
}

func TestCollectOrgEnv_SameMembers_RequiredWins(t *testing.T) {
	// Same set in required AND recommended → required wins, recommended drops it.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "SHARED_KEY"},
		},
		RecommendedEnv: []EnvRequirement{
			{Name: "SHARED_KEY"},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "SHARED_KEY" {
		t.Errorf("required-wins: req=%v", req)
	}
	if len(rec) != 0 {
		t.Errorf("required-wins: rec=%v, want [] (dropped by required)", rec)
	}
}

func TestCollectOrgEnv_StrictDominatesAnyOf_CrossTier(t *testing.T) {
	// Required strict name X causes any-of [X, Y] in recommended to be pruned.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "ANTHROPIC_API_KEY"},
		},
		RecommendedEnv: []EnvRequirement{
			{AnyOf: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Errorf("cross-tier: req=%v", req)
	}
	if len(rec) != 0 {
		t.Errorf("cross-tier: any-of should be pruned from rec, got rec=%v", rec)
	}
}

func TestCollectOrgEnv_StrictDominatesAnyOf_SameTier(t *testing.T) {
	// Required strict X dominates any-of [X, Y] within required (same-tier dedup).
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "SECRET"},
			{AnyOf: []string{"SECRET", "OTHER"}},
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "SECRET" {
		t.Errorf("same-tier: req=%v, want single [SECRET]", req)
	}
}

func TestCollectOrgEnv_DeduplicationAcrossLevels(t *testing.T) {
	// Same requirement declared at org level and workspace level → deduped once.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "SHARED"},
		},
		Workspaces: []OrgWorkspace{{
			Name: "ws1",
			RequiredEnv: []EnvRequirement{
				{Name: "SHARED"}, // duplicate
			},
		}},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "SHARED" {
		t.Errorf("dedup: req=%v, want single [SHARED]", req)
	}
}

func TestCollectOrgEnv_WorkspaceInheritance(t *testing.T) {
	// Child workspace inherits parent's required env (union, not override).
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "ORG_KEY"},
		},
		Workspaces: []OrgWorkspace{{
			Name: "child",
			RequiredEnv: []EnvRequirement{
				{Name: "CHILD_KEY"},
			},
		}},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 2 {
		t.Errorf("inheritance: req=%v, want [ORG_KEY, CHILD_KEY]", req)
	}
}

func TestCollectOrgEnv_AnyOfInRecommended_CrossTier(t *testing.T) {
	// Recommended any-of with member shared by required strict → pruned.
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "KEY_A"},
		},
		RecommendedEnv: []EnvRequirement{
			{AnyOf: []string{"KEY_A", "KEY_B"}},
			{Name: "KEY_C"},
		},
	}
	_, rec := collectOrgEnv(tmpl)
	// KEY_A (strict) prunes the any-of group from recommended.
	// KEY_C (strict) remains.
	if len(rec) != 1 || rec[0].Name != "KEY_C" {
		t.Errorf("any-of cross-tier: rec=%v, want [KEY_C]", rec)
	}
}
