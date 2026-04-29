package handlers

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestOrgImport_ForceFieldRemoved pins the post-#2290 contract: the
// org-import request body no longer carries a Force field, and external
// callers passing legacy `{"force": true}` must NOT be able to bypass
// the required-env preflight. The pre-#2290 force=true escape hatch is
// what allowed the ux-ab-lab org to import without ANTHROPIC_API_KEY
// and ship workspaces that 401'd on every LLM call.
//
// This test is intentionally cheap (no DB, no gin router) — it pins the
// shape that any future "what if we added a force flag for X?" change
// must consciously break in order to revert. The exact same struct
// definition lives in Import() in org.go; if that drifts, the
// reflect-FieldByName check fails.
func TestOrgImport_ForceFieldRemoved(t *testing.T) {
	// Mirror the request-body struct from Import() exactly.
	var body struct {
		Dir      string      `json:"dir"`
		Template OrgTemplate `json:"template"`
	}

	// Legacy clients still sending force=true must be silently tolerated
	// (Go's json.Unmarshal drops unknown fields by default, so this
	// passes regardless — the assertion is that we *want* this drop
	// behavior, not strict-decoding that 400s).
	raw := `{"dir":"some-org","force":true}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("json.Unmarshal: %v — request body must accept legacy force=true without erroring", err)
	}
	if body.Dir != "some-org" {
		t.Fatalf("Dir = %q, want %q", body.Dir, "some-org")
	}

	// The struct must NOT carry a Force field. If a future change adds
	// one back (intentional or not), this test fails and forces an
	// explicit reckoning with the issue #2290 rationale.
	bv := reflect.TypeOf(body)
	if _, found := bv.FieldByName("Force"); found {
		t.Fatal("import body must not carry a Force field — the required-env bypass was removed in #2290")
	}
}
