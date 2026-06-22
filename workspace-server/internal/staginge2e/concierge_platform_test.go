//go:build staging_e2e

package staginge2e

// concierge_platform_test.go — live, against-real-staging coverage for the
// concierge / platform-agent feature set (RFC docs/design/rfc-platform-agent.md
// + the kind/user_tasks/billing-mode/config-tab work). Complements the existing
// workspace-lifecycle + data-persistence staging suites by reusing their
// harness (requireStagingEnv / adminCreateOrg / tenantAdminToken /
// tenantCreateWorkspace / doTenantJSON / jsonField / waitForHTTP — all defined
// in workspace_lifecycle_test.go). NOTHING here re-implements org provisioning
// or teardown: it provisions ONE throwaway tenant, then drives the concierge
// surfaces against the live tenant ws-server and asserts OBSERVABLE state, not
// just HTTP 200.
//
// Features covered (the SaaS-applicable concierge set):
//
//  1. Platform-agent install + identity
//     POST /admin/org/platform-agent makes the org root kind='platform'.
//     GET  /org/identity returns the org name + platform_managed_available.
//     The platform agent becomes the SOLE parent_id IS NULL root, and the
//     ordinary workspace created earlier is re-parented under it.
//
//  2. kind on the workspace API
//     GET /workspaces + GET /workspaces/:id return kind ('platform' for the
//     concierge, 'workspace' for ordinary). Asserts the concierge row's
//     kind=='platform' and the ordinary row's kind=='workspace'.
//
//  4. Discovery peers admin auth (regression guard)
//     GET /registry/:id/peers accepts the operator/tenant ADMIN_TOKEN for the
//     platform agent (was 401 before validateDiscoveryCaller's admin fallback;
//     discovery.go). A 401 here is the exact regression this guards.
//
//  5. BYOK billing
//     GET/PUT /admin/workspaces/:id/llm-billing-mode round-trips
//     (platform_managed → byok → clear), asserting resolved_mode each step.
//
//  6. Config-tab endpoint sweep for the platform agent
//     The per-workspace canvas config tabs (traces / plugins / schedules /
//     channels / secrets / model + peers) must return non-401 for the concierge
//     with the operator token. The concierge is a kind='platform' row with no
//     per-workspace token of its own, so this pins that the admin bearer
//     authenticates every tab (the class validateDiscoveryCaller's admin
//     fallback fixed, extended to the whole tab set).
//
// (Feature 3 — the user_tasks REST+MCP+authz primitive — is covered end-to-end
// against real staging by tests/e2e/test_staging_concierge_e2e.sh, which reuses
// the same CP org-provision/teardown scaffolding from the shell harness. The
// MCP tools/call envelope is shell-shaped, so it lives in the shell suite next
// to the existing local test_user_tasks_e2e.sh rather than being re-encoded in
// Go.)
//
// Guarded by the staging_e2e build tag + STAGING_E2E=1 env gate. Teardown is
// t.Cleanup-driven (admin DELETE /cp/admin/tenants), so a failed assertion can
// never leak the tenant.

import (
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestConciergePlatformAgent_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := fmt.Sprintf("e2e-cncrg-%d", time.Now().Unix()%100000000)
	t.Logf("concierge-platform: slug=%s", slug)

	// --- Step 1: provision throwaway org + tenant (reused scaffolding) ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	t.Logf("org created: org_id=%s", orgID)

	token := tenantAdminToken(t, cfg, slug)
	host := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)

	// An ordinary workspace — its re-parenting under the platform agent is the
	// observable proof that install actually anchored the org tree. We do NOT
	// wait for it to boot online: every assertion here is DB/handler state
	// (kind, parent_id, billing-mode, tab reachability), none of which needs a
	// live container, so skipping the ~10-min EC2 cold-boot keeps the test fast
	// and decoupled from boot-flake.
	ordinaryWS := tenantCreateWorkspace(t, cfg, host, token, orgID)
	t.Logf("ordinary workspace created: %s", ordinaryWS)

	// ── Feature 1: platform-agent install + identity ─────────────────────────
	var platformID string
	t.Run("platform_agent_install_and_identity", func(t *testing.T) {
		// Reuse an existing platform agent if the deployed CP already installed
		// one at org-provision time (the install is idempotent only for the SAME
		// id — installing a DIFFERENT id while one exists would try to re-parent a
		// kind='platform' root and trip workspaces_platform_root_check). So
		// discover first, then install with the discovered-or-fresh id.
		existing := findPlatformRoot(t, host, token, orgID)
		if existing != "" {
			platformID = existing
			t.Logf("platform agent already present (CP-installed): %s — install will be a no-op", platformID)
		} else {
			platformID = newUUIDv4(t)
			t.Logf("no platform agent yet — installing %s", platformID)
		}

		body := fmt.Sprintf(`{"id":%q,"name":%q}`, platformID, "E2E Concierge")
		status, resp := doTenantJSON(t, "POST",
			"https://"+host+"/admin/org/platform-agent", token, orgID, body)
		if status != http.StatusOK {
			t.Fatalf("InstallPlatformAgent: HTTP %d: %s", status, resp)
		}
		if got := jsonField(resp, "status"); got != "installed" {
			t.Fatalf("install: status=%q (want installed): %s", got, resp)
		}
		if got := jsonField(resp, "kind"); got != "platform" {
			t.Fatalf("install: kind=%q (want platform): %s", got, resp)
		}
		if got := jsonField(resp, "platform_agent_id"); got != platformID {
			t.Fatalf("install: platform_agent_id=%q (want %q): %s", got, platformID, resp)
		}

		// Idempotent: a second call must also succeed (installed), proving repeat
		// installs are safe (CP backfill re-runs).
		status2, resp2 := doTenantJSON(t, "POST",
			"https://"+host+"/admin/org/platform-agent", token, orgID, body)
		if status2 != http.StatusOK || jsonField(resp2, "status") != "installed" {
			t.Fatalf("install (idempotent re-run): HTTP %d: %s", status2, resp2)
		}

		// GET /org/identity — open (handler ignores auth: it reads env). BUT it
		// is NOT on the TenantGuard allowlist (only /health, /buildinfo, /metrics,
		// /registry/{register,heartbeat} are — tenant_guard.go), so on a real SaaS
		// tenant it still needs the X-Molecule-Org-Id routing header or
		// TenantGuard 400s TENANT_ORG_HEADER_REQUIRED. Use doTenantJSON (adds the
		// org-id + Origin) — the admin bearer it also sends is harmless here.
		idStatus, idBody := doTenantJSON(t, "GET", "https://"+host+"/org/identity", token, orgID, "")
		if idStatus != http.StatusOK {
			t.Fatalf("GET /org/identity: HTTP %d: %s", idStatus, idBody)
		}
		// platform_managed_available is a bool — assert the field is PRESENT and
		// parseable (its value is environment-dependent: true on a SaaS tenant
		// with the proxy wired, but we don't pin the value, only its presence +
		// type so the canvas pre-login read can't silently lose the field).
		if _, ok := jsonBool(idBody, "platform_managed_available"); !ok {
			t.Fatalf("GET /org/identity missing/non-bool platform_managed_available: %s", idBody)
		}
		// name is a string field (may be "" if MOLECULE_ORG_NAME unset on the
		// tenant) — assert it's present as a JSON key.
		if !strings.Contains(idBody, `"name"`) {
			t.Fatalf("GET /org/identity missing name field: %s", idBody)
		}
		t.Logf("/org/identity: %s", strings.TrimSpace(idBody))

		// The platform agent must now be the SOLE parent_id IS NULL root.
		roots := listRoots(t, host, token, orgID)
		if len(roots) != 1 {
			t.Fatalf("expected exactly ONE parent_id IS NULL root after install, got %d: %v", len(roots), roots)
		}
		if roots[0] != platformID {
			t.Fatalf("sole root is %q, expected the platform agent %q", roots[0], platformID)
		}

		// The ordinary workspace must have been re-parented UNDER the platform
		// agent (it is no longer a root; its parent_id is the platform agent).
		parent := workspaceParentID(t, host, token, orgID, ordinaryWS)
		if parent != platformID {
			t.Fatalf("ordinary workspace %s parent_id=%q, expected to be re-parented under platform agent %q",
				ordinaryWS, parent, platformID)
		}
		t.Logf("platform agent %s is sole root; ordinary ws re-parented under it", platformID)
	})

	if platformID == "" {
		t.Fatal("platform agent id not established by Feature 1 — dependent subtests cannot run")
	}

	// ── Feature 2: kind on the workspace API ─────────────────────────────────
	t.Run("kind_on_workspace_api", func(t *testing.T) {
		// Single GET /workspaces/:id on the concierge → kind='platform'.
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+platformID, token, orgID, "")
		if hs != http.StatusOK {
			t.Fatalf("GET /workspaces/%s: HTTP %d: %s", platformID, hs, body)
		}
		if k := jsonField(body, "kind"); k != "platform" {
			t.Fatalf("concierge GET /workspaces/:id kind=%q (want platform): %s", k, body)
		}

		// Single GET on the ordinary workspace → kind='workspace'.
		hs, body = doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+ordinaryWS, token, orgID, "")
		if hs != http.StatusOK {
			t.Fatalf("GET /workspaces/%s: HTTP %d: %s", ordinaryWS, hs, body)
		}
		if k := jsonField(body, "kind"); k != "workspace" {
			t.Fatalf("ordinary GET /workspaces/:id kind=%q (want workspace): %s", k, body)
		}

		// List GET /workspaces → the concierge row carries kind='platform'.
		hs, listBody := doTenantJSON(t, "GET", "https://"+host+"/workspaces", token, orgID, "")
		if hs != http.StatusOK {
			t.Fatalf("GET /workspaces: HTTP %d: %s", hs, listBody)
		}
		rows := parseWorkspaceList(t, listBody)
		gotPlatform := false
		for _, r := range rows {
			if r.ID == platformID {
				if r.Kind != "platform" {
					t.Fatalf("concierge row in list has kind=%q (want platform): %+v", r.Kind, r)
				}
				gotPlatform = true
			}
			if r.ID == ordinaryWS && r.Kind != "workspace" {
				t.Fatalf("ordinary row in list has kind=%q (want workspace): %+v", r.Kind, r)
			}
		}
		if !gotPlatform {
			t.Fatalf("concierge %s not present in GET /workspaces list (%d rows)", platformID, len(rows))
		}
		t.Logf("kind discriminator correct on list + single for concierge and ordinary ws")
	})

	// ── Feature 2b: model endpoint contract (core#2594) ──────────────────────
	t.Run("concierge_model_endpoint_contract", func(t *testing.T) {
		// GET /workspaces/:id/model must return 200 with a "source" field on the
		// concierge. core#2594 changed the empty-state source from "default" to
		// "unresolved" (the platform no longer substitutes a default model); when
		// the concierge has been provisioned it instead reports its declared model
		// from workspace_secrets. This is a boot-free contract pin — the RESOLVED
		// value (model == the declared model after a provision) is verified live
		// post-deploy, not here, to avoid coupling CI to a concierge container
		// boot (the cp#245 boot-timeout flake class).
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+platformID+"/model", token, orgID, "")
		if hs != http.StatusOK {
			t.Fatalf("GET /workspaces/%s/model: HTTP %d (want 200): %s", platformID, hs, body)
		}
		src := jsonField(body, "source")
		if src == "" {
			t.Fatalf("GET /model returned 200 but no 'source' field: %s", body)
		}
		// Must be one of the two legitimate sources — and crucially NOT the
		// retired "default" (which implied a silent fallback that no longer exists).
		if src != "unresolved" && src != "workspace_secrets" {
			t.Fatalf("GET /model source=%q — want 'unresolved' (not yet provisioned) or "+
				"'workspace_secrets' (declared model persisted); 'default' was retired in core#2594: %s", src, body)
		}
		// If a model IS reported, the source must be the stored secret (never a
		// guessed default) — fail-closed visibility.
		if m := jsonField(body, "model"); m != "" && src != "workspace_secrets" {
			t.Fatalf("GET /model reports model=%q but source=%q (a non-empty model must come from workspace_secrets): %s", m, src, body)
		}
		t.Logf("concierge /model contract OK (source=%q, model=%q)", src, jsonField(body, "model"))
	})

	// ── Feature 4: discovery peers admin auth (regression guard) ─────────────
	t.Run("peers_admin_auth", func(t *testing.T) {
		// The operator/tenant admin token MUST authenticate the platform agent's
		// peer list. Before validateDiscoveryCaller's admin fallback this 401'd
		// (the admin bearer fell through to the per-workspace ValidateToken, which
		// the concierge holds no token for). A 401/403 here is THE regression.
		hs, body := doTenantJSON(t, "GET",
			"https://"+host+"/registry/"+platformID+"/peers", token, orgID, "")
		if hs == http.StatusUnauthorized || hs == http.StatusForbidden {
			t.Fatalf("REGRESSION: GET /registry/%s/peers rejected the admin token (HTTP %d) — "+
				"validateDiscoveryCaller admin fallback broken: %s", platformID, hs, body)
		}
		if hs != http.StatusOK {
			t.Fatalf("GET /registry/%s/peers: HTTP %d (want 200): %s", platformID, hs, body)
		}
		// Body must be a JSON array (the peer list shape) — a 200 with a non-array
		// would be a different regression.
		var peers []json.RawMessage
		if err := json.Unmarshal([]byte(body), &peers); err != nil {
			t.Fatalf("GET /registry/%s/peers returned 200 but body is not a JSON array: %v (%s)", platformID, err, body)
		}
		// The ordinary workspace was re-parented under the concierge, so it is a
		// CHILD peer — assert the concierge can actually see it.
		if !strings.Contains(body, ordinaryWS) {
			t.Fatalf("concierge peer list does not include its re-parented child %s: %s", ordinaryWS, body)
		}
		t.Logf("peers admin-auth OK (HTTP %d, %d peer(s), child visible)", hs, len(peers))
	})

	// ── Feature 5: BYOK billing-mode round-trip ──────────────────────────────
	t.Run("byok_billing_mode_roundtrip", func(t *testing.T) {
		base := "https://" + host + "/admin/workspaces/" + ordinaryWS + "/llm-billing-mode"

		// GET current mode — must resolve to a known enum.
		hs, body := doTenantJSON(t, "GET", base, token, orgID, "")
		if hs != http.StatusOK {
			t.Fatalf("GET billing-mode: HTTP %d: %s", hs, body)
		}
		got := jsonField(body, "resolved_mode")
		if got != "platform_managed" && got != "byok" {
			t.Fatalf("GET billing-mode resolved_mode=%q (want platform_managed|byok): %s", got, body)
		}
		t.Logf("initial resolved_mode=%q", got)

		// PUT mode=byok — explicit per-workspace override → resolved byok.
		hs, body = doTenantJSON(t, "PUT", base, token, orgID, `{"mode":"byok"}`)
		if hs != http.StatusOK {
			t.Fatalf("PUT billing-mode byok: HTTP %d: %s", hs, body)
		}
		if got := jsonField(body, "resolved_mode"); got != "byok" {
			t.Fatalf("PUT mode=byok → resolved_mode=%q (want byok): %s", got, body)
		}

		// GET again — the override persists.
		hs, body = doTenantJSON(t, "GET", base, token, orgID, "")
		if hs != http.StatusOK || jsonField(body, "resolved_mode") != "byok" {
			t.Fatalf("GET after PUT byok: HTTP %d resolved_mode=%q: %s", hs, jsonField(body, "resolved_mode"), body)
		}

		// PUT mode=null — clears the override (back to derived/default).
		hs, body = doTenantJSON(t, "PUT", base, token, orgID, `{"mode":null}`)
		if hs != http.StatusOK {
			t.Fatalf("PUT billing-mode null (clear): HTTP %d: %s", hs, body)
		}
		if got := jsonField(body, "resolved_mode"); got != "platform_managed" && got != "byok" {
			t.Fatalf("PUT mode=null → resolved_mode=%q (want a known enum): %s", got, body)
		}

		// An unknown mode string must 400 (validation contract).
		hs, body = doTenantJSON(t, "PUT", base, token, orgID, `{"mode":"banana"}`)
		if hs != http.StatusBadRequest {
			t.Fatalf("PUT mode=banana → HTTP %d (want 400): %s", hs, body)
		}
		t.Logf("billing-mode round-trip (platform_managed→byok→clear) + 400 on invalid mode OK")
	})

	// ── Feature 6: config-tab endpoint sweep for the concierge ───────────────
	t.Run("config_tab_sweep_for_concierge", func(t *testing.T) {
		// Each per-workspace canvas config tab must authenticate the concierge
		// with the operator token (non-401/403). The concierge is a kind=platform
		// row with no per-workspace token, so this pins that the admin bearer is
		// accepted across the WHOLE tab set (not just peers). We assert non-401
		// (and non-403): the data shape varies per tab and an empty/200 list is a
		// valid state — the regression class is auth-rejection, not data.
		tabs := []struct {
			name string
			url  string
		}{
			{"traces", "https://" + host + "/workspaces/" + platformID + "/traces"},
			{"plugins", "https://" + host + "/workspaces/" + platformID + "/plugins"},
			{"schedules", "https://" + host + "/workspaces/" + platformID + "/schedules"},
			{"channels", "https://" + host + "/workspaces/" + platformID + "/channels"},
			{"secrets", "https://" + host + "/workspaces/" + platformID + "/secrets"},
			{"model", "https://" + host + "/workspaces/" + platformID + "/model"},
			// peers lives off /registry, not /workspaces/:id — re-asserted in the
			// sweep so the whole concierge tab set is covered in one place.
			{"peers", "https://" + host + "/registry/" + platformID + "/peers"},
		}
		for _, tab := range tabs {
			url := tab.url
			hs, body := doTenantJSON(t, "GET", url, token, orgID, "")
			if hs == http.StatusUnauthorized || hs == http.StatusForbidden {
				t.Fatalf("config-tab %q for concierge rejected admin token (HTTP %d) — "+
					"operator must read every config tab for the platform agent: %s", tab.name, hs, body)
			}
			// A 5xx is a real server fault, not an auth issue — surface it too so
			// a broken tab handler doesn't read as "auth OK".
			if hs >= 500 {
				t.Fatalf("config-tab %q for concierge returned HTTP %d (server fault): %s", tab.name, hs, body)
			}
			t.Logf("    tab %-10s → HTTP %d (non-401 ✓)", tab.name, hs)
		}
		t.Logf("all concierge config tabs authenticate the operator token (non-401)")
	})
}

// ─── helpers (concierge-specific; the lifecycle suite owns the shared ones) ──

// workspaceListRow is a flat view of one GET /workspaces row — just the fields
// the concierge assertions read. Populated field-by-field via rawString (the
// row is decoded into a permissive map first, NOT struct-unmarshaled), because
// parent_id arrives as JSON null on a root and a plain string-typed struct field
// would fail that.
type workspaceListRow struct {
	ID       string
	Kind     string
	ParentID string
}

// parseWorkspaceList decodes GET /workspaces (a JSON array). parent_id can be
// null in JSON, which would fail a string-typed field; decode into a permissive
// map and coerce.
func parseWorkspaceList(t *testing.T, body string) []workspaceListRow {
	t.Helper()
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("GET /workspaces body not a JSON array: %v (%s)", err, truncate(body, 300))
	}
	out := make([]workspaceListRow, 0, len(raw))
	for _, m := range raw {
		out = append(out, workspaceListRow{
			ID:       rawString(m["id"]),
			Kind:     rawString(m["kind"]),
			ParentID: rawString(m["parent_id"]),
		})
	}
	return out
}

// listRoots returns the ids of every workspace with parent_id IS NULL.
func listRoots(t *testing.T, host, token, orgID string) []string {
	t.Helper()
	hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces", token, orgID, "")
	if hs != http.StatusOK {
		t.Fatalf("listRoots GET /workspaces: HTTP %d: %s", hs, body)
	}
	var roots []string
	for _, r := range parseWorkspaceList(t, body) {
		if r.ParentID == "" {
			roots = append(roots, r.ID)
		}
	}
	return roots
}

// findPlatformRoot returns the id of the existing kind='platform' root, or ""
// if none is installed yet.
func findPlatformRoot(t *testing.T, host, token, orgID string) string {
	t.Helper()
	hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces", token, orgID, "")
	if hs != http.StatusOK {
		t.Fatalf("findPlatformRoot GET /workspaces: HTTP %d: %s", hs, body)
	}
	for _, r := range parseWorkspaceList(t, body) {
		if r.Kind == "platform" && r.ParentID == "" {
			return r.ID
		}
	}
	return ""
}

// workspaceParentID returns the parent_id of a single workspace ("" when root).
func workspaceParentID(t *testing.T, host, token, orgID, wsID string) string {
	t.Helper()
	hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+wsID, token, orgID, "")
	if hs != http.StatusOK {
		t.Fatalf("workspaceParentID GET /workspaces/%s: HTTP %d: %s", wsID, hs, body)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("workspaceParentID: body not JSON object: %v (%s)", err, truncate(body, 200))
	}
	return rawString(m["parent_id"])
}

// jsonBool extracts a top-level bool field. ok=false when the field is absent or
// not a JSON bool — so the caller can assert presence AND type in one check.
func jsonBool(body, key string) (val bool, ok bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false, false
	}
	raw, present := m[key]
	if !present {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	return b, true
}

// rawString coerces a json.RawMessage to a Go string: a JSON string → its value,
// JSON null/absent → "". Non-string/non-null raws fall back to their literal
// text (sufficient for the id/kind fields we read, which are always strings).
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if string(raw) == "null" {
		return ""
	}
	return strings.Trim(string(raw), `"`)
}

// newUUIDv4 returns a random uuidv4 string — used to mint a platform-agent id
// when the tenant has no CP-installed concierge yet. Dependency-free (the suite
// avoids importing google/uuid into the test-only file).
func newUUIDv4(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		t.Fatalf("newUUIDv4: rand read failed: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
