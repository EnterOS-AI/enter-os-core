package handlers

import (
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// countWorkspaces tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCountWorkspaces_Empty(t *testing.T) {
	got := countWorkspaces(nil)
	if got != 0 {
		t.Errorf("nil: got %d, want 0", got)
	}
	got = countWorkspaces([]OrgWorkspace{})
	if got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}
}

func TestCountWorkspaces_Flat(t *testing.T) {
	tree := []OrgWorkspace{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}
	got := countWorkspaces(tree)
	if got != 3 {
		t.Errorf("flat 3: got %d, want 3", got)
	}
}

func TestCountWorkspaces_Nested(t *testing.T) {
	//        root (1)
	//       /  |  \  (3 children)
	//      c1  c2  c3
	//      |        |
	//      g1      g2 (2 grandchildren)
	tree := []OrgWorkspace{
		{
			Name: "root",
			Children: []OrgWorkspace{
				{Name: "child1", Children: []OrgWorkspace{{Name: "grandchild1"}}},
				{Name: "child2"},
				{Name: "child3", Children: []OrgWorkspace{{Name: "grandchild2"}}},
			},
		},
	}
	got := countWorkspaces(tree)
	if got != 6 {
		t.Errorf("nested: got %d, want 6 (1 root + 3 children + 2 grandchildren)", got)
	}
}

func TestCountWorkspaces_DeepNesting(t *testing.T) {
	// chain of 5 levels
	deep := []OrgWorkspace{
		{Name: "L1", Children: []OrgWorkspace{
			{Name: "L2", Children: []OrgWorkspace{
				{Name: "L3", Children: []OrgWorkspace{
					{Name: "L4", Children: []OrgWorkspace{
						{Name: "L5"},
					}},
				}},
			}},
		}},
	}
	got := countWorkspaces(deep)
	if got != 5 {
		t.Errorf("deep chain: got %d, want 5", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// envRequirementKey tests
// ─────────────────────────────────────────────────────────────────────────────

func TestEnvRequirementKey_SingleMember(t *testing.T) {
	got := envRequirementKey([]string{"API_KEY"})
	if got != "API_KEY" {
		t.Errorf("single: got %q, want %q", got, "API_KEY")
	}
}

func TestEnvRequirementKey_TwoMembers_OrderInsensitive(t *testing.T) {
	keyAB := envRequirementKey([]string{"A", "B"})
	keyBA := envRequirementKey([]string{"B", "A"})
	if keyAB != keyBA {
		t.Errorf("order-insensitive: [A,B]=%q, [B,A]=%q — must match", keyAB, keyBA)
	}
}

func TestEnvRequirementKey_ThreeMembers_Sorted(t *testing.T) {
	key := envRequirementKey([]string{"Z", "A", "M"})
	// Should be "A\x00M\x00Z"
	want := "A\x00M\x00Z"
	if key != want {
		t.Errorf("three members sorted: got %q, want %q", key, want)
	}
}

func TestEnvRequirementKey_EmptyMembers(t *testing.T) {
	got := envRequirementKey(nil)
	if got != "" {
		t.Errorf("nil: got %q, want empty", got)
	}
	got = envRequirementKey([]string{})
	if got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

func TestEnvRequirementKey_DuplicateMembers(t *testing.T) {
	// Duplicates should be preserved in sort; join still works
	key := envRequirementKey([]string{"A", "A", "B"})
	want := "A\x00A\x00B"
	if key != want {
		t.Errorf("duplicates: got %q, want %q", key, want)
	}
}

func TestEnvRequirementKey_UsedForDedup(t *testing.T) {
	// Real dedup case: {A,B} and {B,A} produce same key → dedup-eligible
	// {A,B,C} produces a different key
	keyAB := envRequirementKey([]string{"A", "B"})
	keyBA := envRequirementKey([]string{"B", "A"})
	keyABC := envRequirementKey([]string{"A", "B", "C"})
	if keyAB != keyBA {
		t.Errorf("AB vs BA: keys must match for dedup")
	}
	if keyAB == keyABC {
		t.Errorf("AB vs ABC: keys must differ")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sanitizeEnvMembers tests
// ─────────────────────────────────────────────────────────────────────────────
// envVarNamePattern = ^[A-Z][A-Z0-9_]{0,127}$

func TestSanitizeEnvMembers_AllValid(t *testing.T) {
	members := []string{"API_KEY", "MY_VAR_2", "A"}
	got, ok := sanitizeEnvMembers(members, "test")
	if !ok {
		t.Error("all valid: ok should be true")
	}
	if len(got) != len(members) {
		t.Errorf("all valid: got %v, want %v", got, members)
	}
}

func TestSanitizeEnvMembers_SomeInvalid(t *testing.T) {
	// Lowercase first char — invalid
	members := []string{"API_KEY", "lowercase", "MY_VAR"}
	got, ok := sanitizeEnvMembers(members, "test")
	if !ok {
		t.Error("one invalid: ok should be true (valid members remain)")
	}
	want := []string{"API_KEY", "MY_VAR"}
	if len(got) != len(want) {
		t.Errorf("one invalid: got %v, want %v", got, want)
	}
}

func TestSanitizeEnvMembers_AllInvalid_DropsAll(t *testing.T) {
	members := []string{"lowercase", "123_START", ""}
	got, ok := sanitizeEnvMembers(members, "test")
	if ok {
		t.Error("all invalid: ok should be false")
	}
	if len(got) != 0 {
		t.Errorf("all invalid: got %v, want empty", got)
	}
}

func TestSanitizeEnvMembers_EmptyString_Skipped(t *testing.T) {
	// Empty string is filtered but doesn't make ok=false
	members := []string{"API_KEY", "", "MY_VAR"}
	got, ok := sanitizeEnvMembers(members, "test")
	if !ok {
		t.Error("empty string in valid list: ok should be true")
	}
	if len(got) != 2 {
		t.Errorf("empty string filtered: got %v, want [API_KEY, MY_VAR]", got)
	}
}

func TestSanitizeEnvMembers_MaxLength(t *testing.T) {
	// 128 chars: valid (1 prefix + 127 more = 128, all uppercase)
	valid := "A" + strings.Repeat("B", 127)
	got, ok := sanitizeEnvMembers([]string{valid}, "test")
	if !ok {
		t.Errorf("128 char valid: ok should be true, got %v", got)
	}
	// 129 chars: invalid (exceeds {0,127} suffix in regex)
	tooLong := "A" + strings.Repeat("B", 128)
	_, ok = sanitizeEnvMembers([]string{tooLong}, "test")
	if ok {
		t.Error("129 char invalid: ok should be false")
	}
}

func TestSanitizeEnvMembers_DigitsAndUnderscore(t *testing.T) {
	// regex ^[A-Z][A-Z0-9_]{0,127}$ — first char must be A-Z, not underscore
	valid := []string{"A1", "A_2", "HTTP_200_OK", "ABC123"}
	for _, v := range valid {
		got, ok := sanitizeEnvMembers([]string{v}, "test")
		if !ok {
			t.Errorf("should be valid: %q", v)
		}
		if len(got) != 1 || got[0] != v {
			t.Errorf("got %v, want [%q]", got, v)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// flattenAndSortRequirements tests
// ─────────────────────────────────────────────────────────────────────────────

func TestFlattenAndSortRequirements_Empty(t *testing.T) {
	got := flattenAndSortRequirements(map[string]EnvRequirement{})
	if len(got) != 0 {
		t.Errorf("empty: got %d, want 0", len(got))
	}
}

func TestFlattenAndSortRequirements_SingleFirst(t *testing.T) {
	// Singles come before groups; within singles, alphabetical
	reqs := map[string]EnvRequirement{
		envRequirementKey([]string{"ZETA"}):  {Name: "ZETA"},
		envRequirementKey([]string{"ALPHA"}): {Name: "ALPHA"},
	}
	got := flattenAndSortRequirements(reqs)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].Name != "ALPHA" {
		t.Errorf("first: got %q, want ALPHA", got[0].Name)
	}
	if got[1].Name != "ZETA" {
		t.Errorf("second: got %q, want ZETA", got[1].Name)
	}
}

func TestFlattenAndSortRequirements_GroupsAfterSingles(t *testing.T) {
	reqs := map[string]EnvRequirement{
		envRequirementKey([]string{"X"}):      {Name: "X"},                 // single
		envRequirementKey([]string{"A", "B"}): {AnyOf: []string{"A", "B"}}, // group
	}
	got := flattenAndSortRequirements(reqs)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	// Single X comes before any group
	if got[0].Name != "X" {
		t.Errorf("first should be single X: got %+v", got[0])
	}
	if len(got[1].AnyOf) != 2 {
		t.Errorf("second should be group: got %+v", got[1])
	}
}

func TestFlattenAndSortRequirements_GroupsSortedByMemberKey(t *testing.T) {
	// Groups sorted by their member-key (envRequirementKey sorts AnyOf members).
	// {Z,A} → key "A\x00Z"; {B,C} → key "B\x00C". "A..." < "B..." → A,Z group first.
	reqs := map[string]EnvRequirement{
		envRequirementKey([]string{"Z", "A"}): {AnyOf: []string{"Z", "A"}}, // key: A\x00Z
		envRequirementKey([]string{"B", "C"}): {AnyOf: []string{"B", "C"}}, // key: B\x00C
	}
	got := flattenAndSortRequirements(reqs)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	// A\x00Z < B\x00C alphabetically, so the A,Z group sorts first
	if len(got[0].AnyOf) != 2 || got[0].AnyOf[0] != "Z" {
		t.Errorf("first group: got %+v, want [Z,A] (key A\\x00Z sorts before B\\x00C)", got[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// collectOrgEnv tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCollectOrgEnv_SingleRequired(t *testing.T) {
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{{Name: "API_KEY"}},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("got %d required, want 1", len(req))
	}
	if req[0].Name != "API_KEY" {
		t.Errorf("name: got %q, want API_KEY", req[0].Name)
	}
	if len(rec) != 0 {
		t.Errorf("recommended: got %d, want 0", len(rec))
	}
}

func TestCollectOrgEnv_SingleRecommended(t *testing.T) {
	tmpl := &OrgTemplate{
		RecommendedEnv: []EnvRequirement{{Name: "DEBUG"}},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 0 {
		t.Errorf("required: got %d, want 0", len(req))
	}
	if len(rec) != 1 {
		t.Fatalf("got %d recommended, want 1", len(rec))
	}
	if rec[0].Name != "DEBUG" {
		t.Errorf("name: got %q, want DEBUG", rec[0].Name)
	}
}

func TestCollectOrgEnv_AnyOfGroup(t *testing.T) {
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{{AnyOf: []string{"AWS_KEY", "GCP_KEY", "AZURE_KEY"}}},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("got %d, want 1", len(req))
	}
	if len(req[0].AnyOf) != 3 {
		t.Errorf("any_of members: got %v, want [AWS_KEY, GCP_KEY, AZURE_KEY]", req[0].AnyOf)
	}
}

func TestCollectOrgEnv_InvalidNamesFiltered(t *testing.T) {
	// "lowercase" and "" fail envVarNamePattern → silently dropped
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{{AnyOf: []string{"VALID_KEY", "lowercase", ""}}},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("invalid names filtered: got %d, want 1", len(req))
	}
	if len(req[0].AnyOf) != 1 || req[0].AnyOf[0] != "VALID_KEY" {
		t.Errorf("valid names kept: got %v", req[0].AnyOf)
	}
}

func TestCollectOrgEnv_GroupWithOneInvalid_KeepsRest(t *testing.T) {
	// Mixed: one valid + one invalid → valid member is kept, invalid dropped
	// regex requires ^[A-Z][A-Z0-9_]* — lowercase names are invalid
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{{AnyOf: []string{"GOOD_KEY", "lowercase_invalid"}}},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("got %d, want 1", len(req))
	}
	if len(req[0].AnyOf) != 1 || req[0].AnyOf[0] != "GOOD_KEY" {
		t.Errorf("kept valid member: got %v, want [GOOD_KEY]", req[0].AnyOf)
	}
}

func TestCollectOrgEnv_AllInvalidGroup_Dropped(t *testing.T) {
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{{AnyOf: []string{"lowercase", ""}}},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 0 {
		t.Errorf("all-invalid group: got %d, want 0", len(req))
	}
}

func TestCollectOrgEnv_RequiredSingleDominatesAnyOfGroup(t *testing.T) {
	// Required: API_KEY (strict)
	// Required: any_of [API_KEY, ALT_KEY]
	// → the any_of group is redundant (API_KEY satisfies it already)
	// → any_of group should be dropped from required
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "API_KEY"},
			{AnyOf: []string{"API_KEY", "ALT_KEY"}},
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("strict dominates group: got %d entries, want 1", len(req))
	}
	if req[0].Name != "API_KEY" {
		t.Errorf("strict: got %+v, want name=API_KEY", req[0])
	}
}

func TestCollectOrgEnv_RequiredSingleDominatesRecommendedAnyOf(t *testing.T) {
	// Required: FOO (strict)
	// Recommended: any_of [FOO, BAR]
	// → FOO is already required; the recommended any_of is redundant
	// → recommended any_of should be dropped
	tmpl := &OrgTemplate{
		RequiredEnv:    []EnvRequirement{{Name: "FOO"}},
		RecommendedEnv: []EnvRequirement{{AnyOf: []string{"FOO", "BAR"}}},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 || req[0].Name != "FOO" {
		t.Errorf("required: got %+v", req)
	}
	if len(rec) != 0 {
		t.Errorf("recommended any_of dominated by strict: got %d, want 0", len(rec))
	}
}

func TestCollectOrgEnv_SameTierStrictDominatesGroup(t *testing.T) {
	// Both in required: X (strict), any_of [X, Y] (group)
	// Strict X makes the any_of redundant within the same tier
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "X"},
			{AnyOf: []string{"X", "Y"}},
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("got %d, want 1", len(req))
	}
	if req[0].Name != "X" {
		t.Errorf("strict dominates same-tier group: got %+v", req[0])
	}
}

func TestCollectOrgEnv_WorkspaceLevel(t *testing.T) {
	// Workspaces can also declare required/recommended env
	tmpl := &OrgTemplate{
		Workspaces: []OrgWorkspace{
			{
				Name:           "Dev",
				RequiredEnv:    []EnvRequirement{{Name: "DEV_KEY"}},
				RecommendedEnv: []EnvRequirement{{Name: "DEV_TOOL"}},
			},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Fatalf("workspace required: got %d, want 1", len(req))
	}
	if req[0].Name != "DEV_KEY" {
		t.Errorf("workspace required: got %v", req[0])
	}
	if len(rec) != 1 {
		t.Fatalf("workspace recommended: got %d, want 1", len(rec))
	}
	if rec[0].Name != "DEV_TOOL" {
		t.Errorf("workspace recommended: got %v", rec[0])
	}
}

func TestCollectOrgEnv_DeepNesting(t *testing.T) {
	// Nested children also contribute env requirements
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{{Name: "ORG_LEVEL"}},
		Workspaces: []OrgWorkspace{
			{
				Name:        "Root",
				RequiredEnv: []EnvRequirement{{Name: "ROOT_LEVEL"}},
				Children: []OrgWorkspace{
					{
						Name:        "Child",
						RequiredEnv: []EnvRequirement{{Name: "CHILD_LEVEL"}},
						Children: []OrgWorkspace{
							{Name: "GrandChild", RecommendedEnv: []EnvRequirement{{Name: "GRANDCHILD_TOOL"}}},
						},
					},
				},
			},
		},
	}
	req, rec := collectOrgEnv(tmpl)
	if len(req) != 3 {
		t.Errorf("3 required levels: got %d: %+v", len(req), req)
	}
	if len(rec) != 1 {
		t.Errorf("1 recommended: got %d: %+v", len(rec), rec)
	}
}

func TestCollectOrgEnv_DedupAcrossTiers(t *testing.T) {
	// Same key declared at org level AND workspace level → deduped to 1
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{{Name: "SHARED"}},
		Workspaces: []OrgWorkspace{
			{Name: "ws", RequiredEnv: []EnvRequirement{{Name: "SHARED"}}},
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Errorf("dedup across tiers: got %d, want 1", len(req))
	}
}

func TestCollectOrgEnv_DedupWithinGroup(t *testing.T) {
	// Same key declared multiple times within required → deduped
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "DUPE"},
			{Name: "DUPE"},
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 1 {
		t.Errorf("dedup within tier: got %d, want 1", len(req))
	}
}

func TestCollectOrgEnv_MixedCasePreservesSort(t *testing.T) {
	// Sort order: singles first (alpha), then groups (by member-key)
	tmpl := &OrgTemplate{
		RequiredEnv: []EnvRequirement{
			{Name: "ZETA"},
			{Name: "ALPHA"},
			{AnyOf: []string{"B", "A"}}, // key: A\x00B
			{AnyOf: []string{"Y", "X"}}, // key: X\x00Y
		},
	}
	req, _ := collectOrgEnv(tmpl)
	if len(req) != 4 {
		t.Fatalf("got %d, want 4", len(req))
	}
	// Singles first
	if req[0].Name != "ALPHA" {
		t.Errorf("single ALPHA first: got %+v", req[0])
	}
	if req[1].Name != "ZETA" {
		t.Errorf("single ZETA second: got %+v", req[1])
	}
	// Groups after singles; A,B (key A\x00B) < X,Y (key X\x00Y)
	if len(req[2].AnyOf) != 2 {
		t.Errorf("third should be group: got %+v", req[2])
	}
	if req[2].AnyOf[0] != "B" { // "B" is first alphabetically in [A,B]
		t.Errorf("A,B group should come first: got %+v", req[2])
	}
}
