//go:build staging_e2e

package staginge2e

// workspace_requests_e2e_test.go — core#2606: a NORMAL (non-platform)
// workspace can raise a task AND an approval into the user's unified inbox
// (the Tasks/Approvals tabs), using its OWN workspace token against the
// wsAuth-gated POST /workspaces/:id/requests — the exact server path the
// runtime's create_request / create_approval bridge tools call.
//
// This complements the runtime unit tests (which pin the tool's payload +
// auth) with a live, against-real-staging proof that the request actually
// lands and surfaces in the org pending view. Reuses the lifecycle harness
// (requireStagingEnv / adminCreateOrg / tenantAdminToken /
// tenantCreateWorkspace / doTenantJSON / jsonField) — no new plumbing.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceCanRaiseTaskAndApprovalToUser(t *testing.T) {
	cfg := requireStagingEnv(t)
	slug := fmt.Sprintf("e2e-req-%d", time.Now().Unix()%100000000)
	t.Logf("workspace-requests: slug=%s", slug)

	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	adminToken := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")

	// A normal workspace (kind defaults to "workspace").
	wsID := tenantCreateWorkspace(t, cfg, tenantHost, adminToken, orgID)
	t.Logf("workspace created: %s", wsID)

	// Mint the workspace's OWN bearer — the request tools authenticate with
	// the workspace token, not the org admin token, so the e2e must too.
	wsToken := mintWorkspaceToken(t, cfg, tenantHost, adminToken, orgID, wsID)

	// Raise a task and an approval to the user via the wsAuth endpoint.
	taskTitle := "e2e-2606 task " + slug
	apprTitle := "e2e-2606 approval " + slug
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "task", taskTitle)
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "approval", apprTitle)

	// Both must surface in the org's unified pending view (what the canvas
	// Tasks/Approvals tabs read). Poll briefly — creation broadcasts are
	// async but the row is committed synchronously, so this is fast.
	requirePending(t, tenantHost, adminToken, orgID, "task", taskTitle)
	requirePending(t, tenantHost, adminToken, orgID, "approval", apprTitle)
	t.Logf("both task + approval from a normal workspace surfaced in the org pending view ✓")
}

// TestExternalRuntimeWorkspaceCanRaiseTaskAndApproval is the EXTERNAL-runtime
// half of the core#2606 coverage: the request/approval surface is SSOT-shared,
// so a workspace on a NON-claude-code runtime (here codex, on the cheap
// platform model openai/gpt-5.4-mini) must be able to raise a task AND an
// approval into the user's unified inbox via the SAME wsAuth-gated
// POST /workspaces/:id/requests endpoint. This closes the gap left by
// TestWorkspaceCanRaiseTaskAndApprovalToUser (which only covered a normal
// claude-code workspace) and proves the endpoint is runtime-agnostic — exactly
// the "external + platform workspaces just inherit it SSOT-ly" design.
func TestExternalRuntimeWorkspaceCanRaiseTaskAndApproval(t *testing.T) {
	cfg := requireStagingEnv(t)
	slug := fmt.Sprintf("e2e-extreq-%d", time.Now().Unix()%100000000)
	t.Logf("external-runtime requests: slug=%s", slug)

	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	adminToken := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")

	// An EXTERNAL runtime workspace (codex) on the cheapest platform model.
	wsID := tenantCreateWorkspaceWithRuntime(t, cfg, tenantHost, adminToken, orgID, "ext-codex", "codex", "openai/gpt-5.4-mini")
	t.Logf("external (codex) workspace created: %s", wsID)

	// The request is raised against the wsAuth endpoint with the workspace's own
	// token — no runtime boot required; the endpoint is what the SSOT a2a-bridge
	// create_request/create_approval tools call regardless of runtime.
	wsToken := mintWorkspaceToken(t, cfg, tenantHost, adminToken, orgID, wsID)

	taskTitle := "e2e-2606 ext task " + slug
	apprTitle := "e2e-2606 ext approval " + slug
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "task", taskTitle)
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "approval", apprTitle)

	requirePending(t, tenantHost, adminToken, orgID, "task", taskTitle)
	requirePending(t, tenantHost, adminToken, orgID, "approval", apprTitle)
	t.Logf("both task + approval from an EXTERNAL (codex) workspace surfaced in the org pending view ✓")
}

// mintWorkspaceToken mints a workspace-scoped bearer via the admin surface.
func mintWorkspaceToken(t *testing.T, cfg stagingCfg, host, adminToken, orgID, wsID string) string {
	t.Helper()
	url := "https://" + host + "/admin/workspaces/" + wsID + "/tokens"
	status, resp := doTenantJSON(t, "POST", url, adminToken, orgID, "{}")
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("mint workspace token: HTTP %d: %s", status, resp)
	}
	tok := jsonField(resp, "auth_token")
	if tok == "" {
		t.Fatalf("mint workspace token: no auth_token in response: %s", resp)
	}
	return tok
}

// raiseRequest POSTs a task/approval to recipient=user as the workspace and
// returns the new request_id. Note recipient_id is EMPTY — the generic "the
// user" — which is exactly what makes the More-Info reply path non-trivial
// (the canvas posts the reply with a concrete author_id, see the More-Info
// e2e below).
func raiseRequest(t *testing.T, host, wsToken, orgID, wsID, kind, title string) string {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID + "/requests"
	body := fmt.Sprintf(
		`{"kind":%q,"recipient_type":"user","recipient_id":"","title":%q,"detail":"raised by a normal workspace (core#2606 e2e)"}`,
		kind, title,
	)
	status, resp := doTenantJSON(t, "POST", url, wsToken, orgID, body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("raise %s request (workspace token, wsAuth): HTTP %d: %s", kind, status, resp)
	}
	id := jsonField(resp, "request_id")
	if id == "" {
		t.Fatalf("raise %s request: no request_id: %s", kind, resp)
	}
	return id
}

// TestWorkspaceRequestMoreInfoFlipsToInfoRequested is the live proof of the
// "agent doesn't receive the More-Info thread" fix. An agent→user request is
// stored with an EMPTY recipient_id; the canvas posts the user's clarification
// reply with a CONCRETE author_id ("admin"/session user). The OLD gate
// (authorID == RecipientID) was "admin" == "" → false, so the request never
// flipped to info_requested and the requester agent was never notified. The
// fixed gate keys off "not the requester", so the same POST must now flip the
// status to info_requested — the server-observable proof that the gate fired
// (and therefore that the requester-agent A2A notification was enqueued).
func TestWorkspaceRequestMoreInfoFlipsToInfoRequested(t *testing.T) {
	cfg := requireStagingEnv(t)
	slug := fmt.Sprintf("e2e-moreinfo-%d", time.Now().Unix()%100000000)
	t.Logf("more-info: slug=%s", slug)

	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	adminToken := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")

	wsID := tenantCreateWorkspace(t, cfg, tenantHost, adminToken, orgID)
	wsToken := mintWorkspaceToken(t, cfg, tenantHost, adminToken, orgID, wsID)

	// Agent raises an approval to the generic user (recipient_id="").
	apprTitle := "e2e-moreinfo approval " + slug
	reqID := raiseRequest(t, tenantHost, wsToken, orgID, wsID, "approval", apprTitle)
	t.Logf("raised request %s", reqID)

	// The user posts a More-Info reply via the admin-gated canvas path with a
	// concrete author_id (exactly what RequestsInbox.tsx sends: author_type
	// "user", author_id = session user_id or the "admin" placeholder).
	msgURL := "https://" + tenantHost + "/requests/" + reqID + "/messages"
	msgBody := `{"author_type":"user","author_id":"admin","body":"which environment do you mean?"}`
	status, resp := doTenantJSON(t, "POST", msgURL, adminToken, orgID, msgBody)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("post more-info message (admin path): HTTP %d: %s", status, resp)
	}

	// GET the request and assert the status flipped to info_requested. Under the
	// pre-fix gate it would still read "pending".
	getURL := "https://" + tenantHost + "/requests/" + reqID
	deadline := time.Now().Add(30 * time.Second)
	var lastStatus string
	for time.Now().Before(deadline) {
		code, body := doTenantJSON(t, "GET", getURL, adminToken, orgID, "")
		if code == http.StatusOK {
			var rt struct {
				Request struct {
					Status string `json:"status"`
				} `json:"request"`
			}
			if json.Unmarshal([]byte(body), &rt) == nil {
				lastStatus = rt.Request.Status
				if lastStatus == "info_requested" {
					t.Logf("request %s flipped to info_requested after user More-Info ✓", reqID)
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("request %s never flipped to info_requested (last status %q) — More-Info gate did not fire", reqID, lastStatus)
}

// requirePending polls GET /requests/pending?kind= until `title` appears.
func requirePending(t *testing.T, host, adminToken, orgID, kind, title string) {
	t.Helper()
	url := "https://" + host + "/requests/pending?kind=" + kind
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		status, resp := doTenantJSON(t, "GET", url, adminToken, orgID, "")
		last = resp
		if status == http.StatusOK {
			var rows []map[string]any
			if json.Unmarshal([]byte(resp), &rows) == nil {
				for _, r := range rows {
					if tt, _ := r["title"].(string); strings.Contains(tt, title) {
						return
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%s request %q never appeared in /requests/pending; last body: %s", kind, title, last)
}
