//go:build staging_e2e

package staginge2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestDataVolumeSurvivesRecreate_Staging closes the data-persistence coverage
// gap flagged in core#2332 (P0.5): "data-volume survives recreate" and
// "snapshot-before-container-swap (/home/agent not wiped)" had NO e2e, and both
// map to a real past incident — feedback_workspace_container_swap_wipes_home_agent:
// on a container swap, only the /configs + /workspace binds (the durable data
// volume, cp#326) survive; the container's own $HOME (/home/agent) is ephemeral
// and is WIPED unless a snapshot is taken BEFORE docker stop+rm+run.
//
// This is the FORWARD half of that incident: prove the durable-data invariant
// holds across a recreate so a future regression that drops the data-volume
// reattach (or that flips a "persist" workspace to ephemeral) fails LOUD here
// instead of silently eating a customer's /workspace state.
//
// What it does, end-to-end, against a real staging tenant:
//  0. Provision a throwaway org + tenant via the CP admin API and acquire the
//     tenant admin token (shared harness — mirrors workspace_lifecycle_test.go).
//  1. Create a workspace with compute.data_persistence="persist" (the durable
//     data-volume choice, internal#734) and wait for it to come ONLINE.
//  2. Write a unique sentinel into /workspace (?root=/workspace) — the data
//     volume per cp#326 — via the tenant Files API.
//  3. Probe the /home/agent (container-$HOME) surface to encode the documented
//     contract for the ephemeral side (see assertAgentHomeContract).
//  4. Trigger a recreate / container-swap on the SAME data volume via
//     POST /workspaces/:id/restart, and wait for ONLINE again.
//  5. Assert the /workspace sentinel SURVIVES (data volume reattached +
//     persisted). This is the load-bearing assertion — a wipe here is the
//     regression we are gating.
//
// Guarded by the staging_e2e build tag and STAGING_E2E=1 env gate. Teardown is
// t.Cleanup-driven (admin DELETE /cp/admin/tenants + DELETE /workspaces/:id).
// Promote-to-required is a CTO call (infra-bound; see doc.go).
func TestDataVolumeSurvivesRecreate_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	// Unique-per-run sentinel so a stale prior run can never make a wiped
	// volume look "survived" (we compare exact content, not mere existence).
	stamp := time.Now().UnixNano()
	relPath := fmt.Sprintf("e2e-persist/%d.sentinel", stamp)

	slug := fmt.Sprintf("e2e-persist-%d", time.Now().Unix()%100000000)
	t.Logf("data-persistence: slug=%s", slug)

	// --- Step 0: provision org + tenant, acquire token + wait TLS ready ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	t.Logf("org created: org_id=%s", orgID)

	token := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", tenantHost)

	sentinel := fmt.Sprintf("data-volume-survives-recreate stamp=%d host=%s", stamp, tenantHost)

	// --- Step 1: create workspace with durable data persistence ---
	wsID := createPersistWorkspace(t, tenantHost, token, orgID, stamp)
	t.Cleanup(func() { deletePersistWorkspace(t, tenantHost, token, orgID, wsID) })
	t.Logf("workspace created: id=%s (data_persistence=persist)", wsID)

	waitForWorkspaceOnline(t, tenantHost, token, orgID, wsID, 20*time.Minute)
	t.Logf("workspace %s ONLINE", wsID)

	// --- Step 2: write the /workspace sentinel (data volume, cp#326) ---
	writeWorkspaceFile(t, tenantHost, token, orgID, wsID, "/workspace", relPath, sentinel)
	t.Logf("wrote /workspace sentinel: root=/workspace path=%s", relPath)

	// Read it straight back so a write that silently no-op'd can't masquerade
	// as a survived-recreate later. This also confirms the EIC write landed on
	// the host data volume before we swap the container out from under it.
	if got := readWorkspaceFile(t, tenantHost, token, orgID, wsID, "/workspace", relPath); got != sentinel {
		t.Fatalf("pre-recreate readback mismatch: wrote %q, read %q", sentinel, got)
	}
	t.Logf("pre-recreate readback OK")

	// --- Step 3: encode the /home/agent (ephemeral container-$HOME) contract ---
	assertAgentHomeContract(t, tenantHost, token, orgID, wsID, stamp)

	// A successful Files write to a SaaS workspace can itself debounce-trigger
	// an auto-restart (internal#624). Settle that window first so our explicit
	// recreate below is the swap we actually measure, not a coalesced one that
	// races our readback.
	settleAutoRestart(t, tenantHost, token, orgID, wsID)

	// --- Step 4: recreate / container-swap on the SAME data volume ---
	// POST /restart is the recreate path: Stop (prune=false ALWAYS for restart,
	// so the data volume is NEVER erased) -> re-provision on the same volume,
	// templates NOT re-applied. See workspace_restart.go runRestartCycle.
	triggerRecreate(t, tenantHost, token, orgID, wsID)
	t.Logf("recreate (container swap) triggered via POST /restart")

	// The swap flips status to 'provisioning'; wait for it to come back ONLINE.
	waitForRecreateThenOnline(t, tenantHost, token, orgID, wsID, 20*time.Minute)
	t.Logf("workspace %s back ONLINE after recreate", wsID)

	// --- Step 5: LOAD-BEARING — the /workspace sentinel must SURVIVE ---
	got := readWorkspaceFile(t, tenantHost, token, orgID, wsID, "/workspace", relPath)
	if got != sentinel {
		t.Fatalf("DATA-VOLUME REGRESSION: /workspace sentinel did NOT survive recreate.\n"+
			"  wrote: %q\n  read:  %q\n"+
			"  This is the cp#326 durable-data-volume invariant: a 'persist' workspace's\n"+
			"  /workspace MUST survive a container swap. A wipe here means the data volume\n"+
			"  was not reattached (or a persist→ephemeral regression). See\n"+
			"  feedback_workspace_container_swap_wipes_home_agent.", sentinel, got)
	}
	t.Logf("PASS: /workspace sentinel SURVIVED recreate — data-volume invariant holds (cp#326)")
}

// assertAgentHomeContract encodes the CORRECT, documented expectation for the
// /home/agent (container-$HOME) side of the incident.
//
// The Files API exposes the container's own $HOME via ?root=/agent-home (the
// docker-exec backend, internal#425 RFC). That backend is intentionally STUBBED
// today: every verb returns 501 Not Implemented. So there is NO supported
// platform write path into the container's /home/agent — which is precisely
// because that directory is EPHEMERAL: it lives inside the container, not on the
// durable data volume, and is WIPED on every container swap unless a snapshot is
// taken first (the incident's snapshot-before-stop+rm+run rule, which is a
// CP-side provisioner concern, not a tenant ws-server file-API surface).
//
// This assertion is the regression tripwire for that contract: if a future
// change wires /agent-home to a path WITHOUT also making it data-volume-backed,
// this 501 flips to 200 and the test fails LOUD — forcing whoever lit up the
// surface to first answer "is /home/agent now durable, and was the snapshot
// hook added?" rather than silently shipping a wipe-on-recreate surface.
//
// We do NOT write-then-recreate-then-expect-wipe on /home/agent: asserting a
// WIPE as a pass would be fail-open (a no-op write would also "pass"). Pinning
// the 501 contract is the fail-closed encoding.
func assertAgentHomeContract(t *testing.T, host, token, orgID, wsID string, stamp int64) {
	t.Helper()
	rel := fmt.Sprintf("e2e-persist/%d.home.sentinel", stamp)
	url := fmt.Sprintf("https://%s/workspaces/%s/files/%s?root=%s",
		host, wsID, rel, "/agent-home")
	status, body := doTenantJSON(t, "PUT", url, token, orgID, fmt.Sprintf(`{"content":%q}`, "x"))

	switch status {
	case http.StatusNotImplemented:
		// Documented contract: container-$HOME browse/write is stubbed BECAUSE
		// it is ephemeral. No durable surface to assert survival on. Good.
		t.Logf("/home/agent contract OK: /agent-home is 501 (ephemeral container-$HOME, no durable write surface — snapshot-before-swap is a CP-side concern)")
	case http.StatusOK:
		// The stub was lit up. This is a contract change that MUST be paired
		// with data-volume backing + a snapshot-before-swap hook; until this
		// test is extended to prove BOTH, treat the bare flip as a regression
		// of the documented ephemeral contract.
		t.Fatalf("CONTRACT DRIFT: PUT ?root=/agent-home returned 200 — the container-$HOME surface was wired up.\n"+
			"  Per feedback_workspace_container_swap_wipes_home_agent, /home/agent is EPHEMERAL and wiped on\n"+
			"  container swap unless snapshotted first. If this surface is now durable, EXTEND this test to\n"+
			"  write→recreate→assert-survival on /home/agent AND assert the snapshot-before-swap hook fired.\n"+
			"  Do not leave a write-able-but-ephemeral surface uncovered. body=%s", body)
	default:
		// 4xx other than 501 (e.g. 400/404) is acceptable — still "not a
		// durable write surface". Anything 5xx that ISN'T 501 is a real bug.
		if status >= 500 {
			t.Fatalf("/home/agent contract probe: unexpected %d (want 501 or a 4xx): %s", status, body)
		}
		t.Logf("/home/agent contract: ?root=/agent-home returned %d (non-durable surface) — acceptable", status)
	}
}

// --- workspace lifecycle over the tenant API ------------------------------

// createPersistWorkspace creates a throwaway workspace with the durable
// data-volume choice (compute.data_persistence="persist", internal#734). The
// "persist" choice is what makes /workspace survive a recreate; we set it
// explicitly rather than relying on the auto/org-flag default so the invariant
// under test is unambiguous.
func createPersistWorkspace(t *testing.T, host, token, orgID string, stamp int64) string {
	t.Helper()
	url := "https://" + host + "/workspaces"
	body := fmt.Sprintf(
		`{"name":%q,"runtime":%q,"tier":%d,"compute":{"data_persistence":%q}}`,
		fmt.Sprintf("e2e-persist-%d", stamp%100000000), "claude-code", 1, "persist",
	)
	status, resp := doTenantJSON(t, "POST", url, token, orgID, body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create workspace: HTTP %d: %s", status, resp)
	}
	id := jsonField(resp, "id")
	if id == "" {
		t.Fatalf("create workspace: no id in response: %s", resp)
	}
	return id
}

// deletePersistWorkspace is the t.Cleanup teardown — best-effort, never fails
// the test. DELETE without prune so a hung delete doesn't strand the test;
// staging sweep reclaims any leftover compute. (The org/tenant itself is torn
// down separately via adminDeleteTenant.)
func deletePersistWorkspace(t *testing.T, host, token, orgID, wsID string) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID
	status, resp := doTenantJSON(t, "DELETE", url, token, orgID, "")
	if status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNoContent && status != http.StatusNotFound {
		t.Logf("WARNING: teardown DELETE workspace %s returned HTTP %d: %s (manual cleanup may be needed)", wsID, status, resp)
		return
	}
	t.Logf("teardown: deleted workspace %s (HTTP %d)", wsID, status)
}

// waitForWorkspaceOnline polls GET /workspaces/:id until .status == "online".
func waitForWorkspaceOnline(t *testing.T, host, token, orgID, wsID string, timeout time.Duration) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		status, body := doTenantJSON(t, "GET", url, token, orgID, "")
		if status == http.StatusOK {
			last = jsonField(body, "status")
			if last == "online" {
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("workspace %s did not reach status=online within %s (last=%q)", wsID, timeout, last)
}

// triggerRecreate POSTs /restart, the recreate / container-swap path. The
// handler tears down the container and re-provisions on the SAME data volume
// (Stop is called with prune=false for restart — see workspace_restart.go's
// cpStopWithRetryErr — so a recreate can NEVER erase the data volume).
func triggerRecreate(t *testing.T, host, token, orgID, wsID string) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID + "/restart"
	status, body := doTenantJSON(t, "POST", url, token, orgID, "")
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("trigger recreate (POST /restart): HTTP %d: %s", status, body)
	}
}

// waitForRecreateThenOnline waits out the swap. The recreate flips status to
// 'provisioning'; we first observe it LEAVE online (so we don't read a stale
// "still online" before the swap starts), then wait for it to return to online.
// If we never catch the provisioning dip (fast swap), the subsequent online
// poll still proves liveness — the load-bearing assertion is the sentinel read,
// not the transient state machine.
func waitForRecreateThenOnline(t *testing.T, host, token, orgID, wsID string, timeout time.Duration) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID
	deadline := time.Now().Add(timeout)

	// Brief window to catch the provisioning dip (best-effort; not required).
	dipDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(dipDeadline) {
		status, body := doTenantJSON(t, "GET", url, token, orgID, "")
		if status == http.StatusOK && jsonField(body, "status") != "online" {
			break
		}
		time.Sleep(3 * time.Second)
	}

	var last string
	for time.Now().Before(deadline) {
		status, body := doTenantJSON(t, "GET", url, token, orgID, "")
		if status == http.StatusOK {
			last = jsonField(body, "status")
			if last == "online" {
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("workspace %s did not return to status=online after recreate within %s (last=%q)", wsID, timeout, last)
}

// settleAutoRestart absorbs the internal#624 file-write→restart debounce so the
// explicit recreate we measure isn't coalesced with an implicit one. The
// debounce window is 15s + a restart cycle; we poll back to a stable online.
func settleAutoRestart(t *testing.T, host, token, orgID, wsID string) {
	t.Helper()
	// Give the debounce window time to fire (or not) ...
	time.Sleep(20 * time.Second)
	// ... then ensure we're back to a stable online before the measured swap.
	waitForWorkspaceOnline(t, host, token, orgID, wsID, 10*time.Minute)
}

// --- tenant Files API ------------------------------------------------------

// writeWorkspaceFile PUTs a file via the tenant Files API into the given root.
// root="/workspace" is the literal data-volume path (cp#326).
func writeWorkspaceFile(t *testing.T, host, token, orgID, wsID, root, relPath, content string) {
	t.Helper()
	url := fmt.Sprintf("https://%s/workspaces/%s/files/%s?root=%s",
		host, wsID, relPath, root)
	status, body := doTenantJSON(t, "PUT", url, token, orgID, fmt.Sprintf(`{"content":%q}`, content))
	if status != http.StatusOK {
		t.Fatalf("write %s%s: HTTP %d: %s", root, relPath, status, body)
	}
}

// readWorkspaceFile GETs a file via the tenant Files API and returns its
// content. Fails the test on any non-200 (a not-found after a recreate is the
// wipe we are gating, so the caller compares content and emits the regression
// message — but a transport/auth failure should still fail loud here).
func readWorkspaceFile(t *testing.T, host, token, orgID, wsID, root, relPath string) string {
	t.Helper()
	url := fmt.Sprintf("https://%s/workspaces/%s/files/%s?root=%s",
		host, wsID, relPath, root)
	status, body := doTenantJSON(t, "GET", url, token, orgID, "")
	if status == http.StatusNotFound {
		// Surface the not-found as empty content; the caller's exact-content
		// compare turns this into the DATA-VOLUME REGRESSION message.
		return ""
	}
	if status != http.StatusOK {
		t.Fatalf("read %s%s: HTTP %d: %s", root, relPath, status, body)
	}
	return jsonField(body, "content")
}
