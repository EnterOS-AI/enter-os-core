//go:build staging_e2e

package staginge2e

// approval_gate_concierge_e2e_test.go — live staging end-to-end coverage for
// core#2574: the Phase-4 approval gate on the admin-token path.
//
// What it proves that unit + integration tests cannot: that against a REAL
// SaaS tenant, the concierge's ADMIN_TOKEN (Tier 2b) is gated when minting
// org API tokens and writing workspace secrets — not just that the Go code
// returns 202, but that the REAL Postgres row is created and the retry-after-
// approval flow works through the actual HTTP surface.
//
// Run with:
//
//	STAGING_E2E=1 CP_BASE_URL=https://cp.staging.moleculesai.app \
//	  CP_ADMIN_API_TOKEN=... go test -tags=staging_e2e ./internal/staginge2e/ \
//	  -run TestConciergeApprovalGate_Staging -v
//
// Guarded by the staging_e2e build tag + STAGING_E2E=1 env gate.
// Teardown is t.Cleanup-driven (admin DELETE /cp/admin/tenants).
//
// ISOLATION NOTE (fix/staging-e2e-approval-gate-subtest-isolation):
// Previously, the test declared `var secretApprovalID string` at parent
// scope and the `secret_write_with_approval_succeeds` subtest read from
// it. If the upstream rejection subtest was skipped or failed before
// reaching the assignment, the approval subtest silently t.Skip'd —
// masking its own code path. The fix factors the gated-write → grant →
// retry → single-use flow into secretWriteApprovalFlow (no parent-scope
// state) and each subtest now exercises its full path independently.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// secretWriteApprovalFlow is a self-contained helper that exercises the
// gated-write → grant → retry → single-use path. It mints its own
// approval_id from a fresh gated write and returns (approvalID,
// freshApprovalID) where approvalID is the one that was granted, and
// freshApprovalID is the SECOND-write approval_id (used to verify the
// consumed approval was single-use).
//
// No parent-scope state. Callers (subtests or top-level tests) can call
// this helper multiple times and each call will run independently with
// its own approval row. Caller passes a unique `key` per call so the
// Postgres secret-write doesn't collide.
func secretWriteApprovalFlow(t *testing.T, host, token, orgID, ordinaryWS, key, value1, value2 string) (approvalID, freshApprovalID string) {
	t.Helper()
	writeURL := "https://" + host + "/workspaces/" + ordinaryWS + "/secrets"

	// 1. Gated write to capture an approval_id.
	status, body := doTenantJSON(t, "POST", writeURL, token, orgID,
		fmt.Sprintf(`{"key":%q,"value":%q}`, key, value1))
	if status != http.StatusAccepted {
		t.Fatalf("gated secret write (key=%s): want 202, got %d: %s", key, status, body)
	}
	if !strings.Contains(body, `"status":"pending_approval"`) {
		t.Fatalf("gated secret write: expected pending_approval in 202 body, got: %s", body)
	}
	if !strings.Contains(body, `"action":"secret_write"`) {
		t.Fatalf("gated secret write: expected action=secret_write in 202 body, got: %s", body)
	}
	approvalID = jsonField(body, "approval_id")
	if approvalID == "" {
		t.Fatalf("gated secret write: approval_id missing from 202 body: %s", body)
	}
	t.Logf("self-contained gated write captured approval_id=%s (key=%s)", approvalID, key)

	// 2. Grant the approval via the decide endpoint.
	decideURL := fmt.Sprintf("https://%s/workspaces/%s/approvals/%s/decide",
		host, ordinaryWS, approvalID)
	status, body = doTenantJSON(t, "POST", decideURL, token, orgID, `{"decision":"approved"}`)
	if status != http.StatusOK {
		t.Fatalf("decide approve (approval_id=%s): HTTP %d: %s", approvalID, status, body)
	}
	if !strings.Contains(body, `"status":"approved"`) {
		t.Fatalf("decide approve: expected approved status, got: %s", body)
	}
	t.Logf("approval %s granted", approvalID)

	// 3. Retry the secret write — must succeed.
	status, body = doTenantJSON(t, "POST", writeURL, token, orgID,
		fmt.Sprintf(`{"key":%q,"value":%q}`, key, value1))
	if status != http.StatusOK {
		t.Fatalf("retry after approval (key=%s): want 200, got %d: %s", key, status, body)
	}
	if !strings.Contains(body, `"status":"saved"`) {
		t.Fatalf("retry after approval: expected saved status, got: %s", body)
	}
	t.Logf("secret write succeeded after approval (200, key=%s)", key)

	// 4. Verify the secret IS now present.
	listStatus, listBody := doTenantJSON(t, "GET", writeURL, token, orgID, "")
	if listStatus != http.StatusOK {
		t.Fatalf("list secrets after approved write: HTTP %d: %s", listStatus, listBody)
	}
	if !strings.Contains(listBody, key) {
		t.Fatalf("%s missing from list after approved write: %s", key, listBody)
	}
	t.Logf("secret %s present in list after approved write", key)

	// 5. Single-use: a SECOND write with the SAME key must create a NEW
	//    pending approval (the first one was consumed).
	status, body = doTenantJSON(t, "POST", writeURL, token, orgID,
		fmt.Sprintf(`{"key":%q,"value":%q}`, key, value2))
	if status != http.StatusAccepted {
		t.Fatalf("second write after consumption (key=%s): want 202, got %d: %s", key, status, body)
	}
	if !strings.Contains(body, `"status":"pending_approval"`) {
		t.Fatalf("second write: expected fresh pending_approval, got: %s", body)
	}
	freshApprovalID = jsonField(body, "approval_id")
	if freshApprovalID == "" {
		t.Fatalf("second write: fresh approval_id missing: %s", body)
	}
	if freshApprovalID == approvalID {
		t.Fatalf("approval_id reused — the consumed approval was not single-use: %s", freshApprovalID)
	}
	t.Logf("single-use verified: second write got fresh approval_id=%s (key=%s)", freshApprovalID, key)
	return approvalID, freshApprovalID
}

func TestConciergeApprovalGate_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := fmt.Sprintf("e2e-gate-%d", time.Now().Unix()%100000000)
	t.Logf("approval-gate: slug=%s", slug)

	// --- Step 1: provision throwaway org + tenant ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	t.Logf("org created: org_id=%s", orgID)

	token := tenantAdminToken(t, cfg, slug)
	host := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)

	// --- Step 2: create an ordinary workspace (secret-write needs a :id) ---
	ordinaryWS := tenantCreateWorkspace(t, cfg, host, token, orgID)
	t.Logf("ordinary workspace created: %s", ordinaryWS)

	// Install platform agent so there is a concierge workspace (kind=platform).
	platformID := findPlatformRoot(t, host, token, orgID)
	if platformID == "" {
		platformID = newUUIDv4(t)
		body := fmt.Sprintf(`{"id":%q,"name":%q}`, platformID, "E2E Gate Concierge")
		status, resp := doTenantJSON(t, "POST",
			"https://"+host+"/admin/org/platform-agent", token, orgID, body)
		if status != http.StatusOK {
			t.Fatalf("install platform agent: HTTP %d: %s", status, resp)
		}
		t.Logf("platform agent installed: %s", platformID)
	} else {
		t.Logf("platform agent already present: %s", platformID)
	}

	// ── Regression (c): concierge admin-token mint WITHOUT approval → 202 ─────
	t.Run("org_token_mint_without_approval_rejected", func(t *testing.T) {
		status, body := doTenantJSON(t, "POST",
			"https://"+host+"/org/tokens", token, orgID, `{"name":"exploit-test-token"}`)
		if status == http.StatusOK {
			var resp map[string]interface{}
			_ = json.Unmarshal([]byte(body), &resp)
			t.Fatalf("REGRESSION core#2574: admin-token mint returned 200 (token created) with ZERO approvals — %v", resp)
		}
		if status != http.StatusAccepted {
			t.Fatalf("admin-token mint: want 202, got %d: %s", status, body)
		}
		if !strings.Contains(body, `"status":"pending_approval"`) {
			t.Fatalf("expected pending_approval in 202 body, got: %s", body)
		}
		if !strings.Contains(body, `"action":"org_token_mint"`) {
			t.Fatalf("expected action=org_token_mint in 202 body, got: %s", body)
		}
		approvalID := jsonField(body, "approval_id")
		if approvalID == "" {
			t.Fatalf("approval_id missing from 202 body: %s", body)
		}
		t.Logf("mint correctly gated (202, approval_id=%s)", approvalID)

		// Verify NO token was actually minted.
		listStatus, listBody := doTenantJSON(t, "GET",
			"https://"+host+"/org/tokens", token, orgID, "")
		if listStatus != http.StatusOK {
			t.Fatalf("list tokens: HTTP %d: %s", listStatus, listBody)
		}
		if strings.Contains(listBody, "exploit-test-token") {
			t.Fatalf("REGRESSION: exploit-test-token appears in live token list despite zero approvals")
		}
		t.Logf("no token minted — list does not contain exploit-test-token")
	})

	// ── (a): concierge admin-token secret write WITHOUT approval → 202 ────────
	// Self-contained: no parent-scope state. The approval_id captured here
	// is local to this subtest. The downstream approval subtest mints its
	// OWN approval_id rather than reading from this one.
	t.Run("secret_write_without_approval_rejected", func(t *testing.T) {
		url := "https://" + host + "/workspaces/" + ordinaryWS + "/secrets"
		status, body := doTenantJSON(t, "POST", url, token, orgID,
			`{"key":"E2E_GATED_SECRET_REJECT","value":"gated-value-123"}`)
		if status == http.StatusOK {
			var resp map[string]interface{}
			_ = json.Unmarshal([]byte(body), &resp)
			t.Fatalf("REGRESSION core#2574: admin-token secret write returned 200 with ZERO approvals — %v", resp)
		}
		if status != http.StatusAccepted {
			t.Fatalf("admin-token secret write: want 202, got %d: %s", status, body)
		}
		if !strings.Contains(body, `"status":"pending_approval"`) {
			t.Fatalf("expected pending_approval in 202 body, got: %s", body)
		}
		if !strings.Contains(body, `"action":"secret_write"`) {
			t.Fatalf("expected action=secret_write in 202 body, got: %s", body)
		}
		approvalID := jsonField(body, "approval_id")
		if approvalID == "" {
			t.Fatalf("approval_id missing from 202 body: %s", body)
		}
		t.Logf("secret write correctly gated (202, approval_id=%s, local to this subtest)", approvalID)

		// Verify the secret was NOT actually written.
		listStatus, listBody := doTenantJSON(t, "GET", url, token, orgID, "")
		if listStatus != http.StatusOK {
			t.Fatalf("list secrets: HTTP %d: %s", listStatus, listBody)
		}
		if strings.Contains(listBody, "E2E_GATED_SECRET_REJECT") {
			t.Fatalf("REGRESSION: E2E_GATED_SECRET_REJECT appears in list despite zero approvals")
		}
		t.Logf("secret not written — list does not contain E2E_GATED_SECRET_REJECT")
	})

	// ── (b): WITH a granted approval, secret write succeeds ───────────────────
	// Self-contained: calls secretWriteApprovalFlow which mints its own
	// approval_id via a fresh gated write. No dependency on the upstream
	// rejection subtest's state. If the upstream subtest were skipped or
	// failed, this one would still run end-to-end.
	t.Run("secret_write_with_approval_succeeds", func(t *testing.T) {
		approvalID, freshApprovalID := secretWriteApprovalFlow(t, host, token, orgID,
			ordinaryWS, "E2E_GATED_SECRET_OK", "gated-value-123", "gated-value-456")
		if approvalID == "" {
			t.Fatalf("self-contained helper returned empty approval_id")
		}
		if freshApprovalID == "" {
			t.Fatalf("self-contained helper returned empty fresh approval_id")
		}
		if freshApprovalID == approvalID {
			t.Fatalf("helper violated single-use: fresh == granted (%s)", freshApprovalID)
		}
	})

	// ── List pending approvals via admin surface ──────────────────────────────
	// The /approvals/pending endpoint is AdminAuth-gated and returns ALL pending
	// approvals across the tenant. After the mint rejection above, there should be
	// at least one pending org_token_mint approval (workspace_id may be "").
	t.Run("pending_approvals_listable_by_admin", func(t *testing.T) {
		status, body := doTenantJSON(t, "GET",
			"https://"+host+"/approvals/pending", token, orgID, "")
		if status != http.StatusOK {
			t.Fatalf("list pending approvals: HTTP %d: %s", status, body)
		}
		if !strings.Contains(body, `"action":"org_token_mint"`) {
			t.Fatalf("expected at least one org_token_mint pending approval in list: %s", body)
		}
		t.Logf("pending approvals list contains org_token_mint (admin can see the gate-fired request)")
	})
}

// TestConciergeApprovalGate_Staging_OrderIndependent demonstrates that the
// secret-write-with-approval flow is now self-contained: the helper
// secretWriteApprovalFlow has no parent-scope state, so calling it twice
// in sequence produces two INDEPENDENT approval_ids and two INDEPENDENT
// secret rows. If the helper were coupled to a parent-scope variable, the
// second call would either (a) reuse the first call's approval_id, (b)
// fail because the first call's secret was already at the target value, or
// (c) produce the same fresh approval_id on the second-call single-use
// check. This test asserts none of those regressions occur.
//
// The test re-runs the full setup (org/tenant/workspace/platform-agent)
// because it stands alone — there is no shared parent scope. Each
// call to secretWriteApprovalFlow uses a unique secret key so the
// Postgres secret-write path doesn't collide.
func TestConciergeApprovalGate_Staging_OrderIndependent(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := fmt.Sprintf("e2e-isolation-%d", time.Now().Unix()%100000000)
	t.Logf("isolation: slug=%s", slug)

	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	t.Logf("org created: org_id=%s", orgID)

	token := tenantAdminToken(t, cfg, slug)
	host := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)

	ordinaryWS := tenantCreateWorkspace(t, cfg, host, token, orgID)
	t.Logf("ordinary workspace created: %s", ordinaryWS)

	// Install platform agent so the tenant is a valid concierge surface.
	platformID := findPlatformRoot(t, host, token, orgID)
	if platformID == "" {
		platformID = newUUIDv4(t)
		body := fmt.Sprintf(`{"id":%q,"name":%q}`, platformID, "E2E Isolation")
		status, resp := doTenantJSON(t, "POST",
			"https://"+host+"/admin/org/platform-agent", token, orgID, body)
		if status != http.StatusOK {
			t.Fatalf("install platform agent: HTTP %d: %s", status, resp)
		}
		t.Logf("platform agent installed: %s", platformID)
	}

	// First call: complete secret-write-with-approval flow, capture the
	// granted approval_id and the fresh second-write approval_id.
	approvalA1, freshA2 := secretWriteApprovalFlow(t, host, token, orgID,
		ordinaryWS, "E2E_ISO_KEY_A", "iso-A-value-1", "iso-A-value-2")
	if approvalA1 == "" || freshA2 == "" {
		t.Fatalf("first helper call returned empty ids (approvalA1=%q, freshA2=%q)", approvalA1, freshA2)
	}
	if approvalA1 == freshA2 {
		t.Fatalf("first helper call violated single-use: approvalA1 == freshA2 == %s", approvalA1)
	}
	t.Logf("first call: approvalA1=%s, freshA2=%s (independent)", approvalA1, freshA2)

	// Second call: same helper, different key, must produce INDEPENDENT
	// approval_ids. If the helper were coupled to a parent-scope variable
	// (the bug being fixed), the second call would either reuse approvalA1
	// or fail because state was already set up.
	approvalB1, freshB2 := secretWriteApprovalFlow(t, host, token, orgID,
		ordinaryWS, "E2E_ISO_KEY_B", "iso-B-value-1", "iso-B-value-2")
	if approvalB1 == "" || freshB2 == "" {
		t.Fatalf("second helper call returned empty ids (approvalB1=%q, freshB2=%q)", approvalB1, freshB2)
	}
	if approvalB1 == freshB2 {
		t.Fatalf("second helper call violated single-use: approvalB1 == freshB2 == %s", approvalB1)
	}
	if approvalB1 == approvalA1 {
		t.Fatalf("isolation violation: second call reused first call's approval_id (%s)", approvalA1)
	}
	if freshB2 == freshA2 {
		t.Fatalf("isolation violation: second call's fresh_id matches first call's fresh_id (%s)", freshA2)
	}
	if approvalB1 == freshA2 || freshB2 == approvalA1 {
		t.Fatalf("isolation violation: cross-call id reuse (approvalA1=%s, freshA2=%s, approvalB1=%s, freshB2=%s)",
			approvalA1, freshA2, approvalB1, freshB2)
	}
	t.Logf("second call: approvalB1=%s, freshB2=%s (no cross-call id reuse)", approvalB1, freshB2)
	t.Logf("isolation verified: helper secretWriteApprovalFlow is self-contained and order-independent")
}
