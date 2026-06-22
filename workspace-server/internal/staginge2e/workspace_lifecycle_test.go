//go:build staging_e2e

package staginge2e

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWorkspaceLifecycle_Staging is the live, against-real-staging end-to-end
// test for core#2332 P1.10 — workspace lifecycle (soft-restart / pause / resume
// / hibernate) coverage.
//
// What it proves that the handler unit tests (httptest in
// internal/handlers/*_test.go) cannot: that against a REAL EC2-backed tenant
// workspace, the lifecycle endpoints actually transition the CONTAINER state
// and recover — not just flip a DB flag or return HTTP 200.
//
// Pipeline:
//
//  1. Provision a throwaway org + tenant via the CP admin API.
//
//  2. Acquire the tenant admin token (accepted by ws-server WorkspaceAuth as
//     ADMIN_TOKEN — see middleware/wsauth_middleware.go).
//
//  3. Create a workspace via the tenant ws-server; wait for status=online with
//     a routable url (the real boot→register signal).
//
//  4. Drive each lifecycle endpoint and assert OBSERVABLE state:
//
//     soft restart (POST /restart):
//     online → provisioning → online, and a post-restart serve probe (A2A
//     round-trip) succeeds — proves the container came back serveable, not
//     just that the row flipped.
//
//     pause (POST /pause):
//     → paused, AND the container is genuinely stopped — observed via the
//     tenant API as: url cleared + the workspace no longer serves A2A
//     (a stopped EC2/container is unreachable; a mere flag would still serve).
//     resume (POST /resume):
//     paused → provisioning → online + serveable again.
//
//     hibernate (POST /hibernate?force=true):
//     online → hibernated, container stopped (url cleared, unserveable).
//     wake (next A2A message):
//     hibernated → online (auto-wake-on-message; Resume only handles paused).
//
// Status is read from the live DB-backed GET /workspaces/:id (canvas) endpoint
// — the response body of the lifecycle POST could lie; the GET proves the row.
//
// Guarded by the staging_e2e build tag and STAGING_E2E=1 env gate. Teardown is
// t.Cleanup-driven (admin DELETE /cp/admin/tenants).
func TestWorkspaceLifecycle_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := fmt.Sprintf("e2e-life-%d", time.Now().Unix()%100000000)
	t.Logf("workspace-lifecycle: slug=%s", slug)

	// --- Step 1: provision org via admin API ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	t.Logf("org created: org_id=%s", orgID)

	// --- Step 1b: acquire tenant admin token + wait for tenant TLS ready ---
	token := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", tenantHost)

	// --- Step 2: create workspace + wait online (routable) ---
	wsID := tenantCreateWorkspace(t, cfg, tenantHost, token, orgID)
	waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "initial boot")
	t.Logf("workspace %s online + routable", wsID)

	// Baseline: the freshly-online workspace must actually serve A2A.
	assertServes(t, tenantHost, token, orgID, wsID, "baseline (post-boot)")

	// ── soft restart ────────────────────────────────────────────────────────
	// online → provisioning → online; container must come back serveable.
	t.Run("restart", func(t *testing.T) {
		status, body := postLifecycle(t, tenantHost, token, orgID, wsID, "/restart")
		if status != http.StatusOK {
			t.Fatalf("restart: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "provisioning" {
			t.Fatalf("restart: body status=%q (expected provisioning): %s", st, body)
		}
		// The endpoint flips status→provisioning synchronously (before the HTTP
		// response) then re-provisions in a goroutine. We don't hard-assert
		// observing the intermediate 'provisioning' via GET: on a fast box the
		// row can race back to online before our first poll, so requiring to
		// CATCH provisioning would be a false-negative flake. The body already
		// proved the synchronous flip; the load-bearing observable is the
		// eventual online+routable + a successful serve probe below.
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "restart→online")
		// Post-restart liveness/serve probe — proves the container is actually
		// back, not just that the status row says online.
		assertServes(t, tenantHost, token, orgID, wsID, "post-restart")
		t.Logf("restart VERIFIED: online → provisioning → online + serveable")
	})

	// ── pause → resume ──────────────────────────────────────────────────────
	t.Run("pause_resume", func(t *testing.T) {
		// pause → paused, container genuinely stopped.
		status, body := postLifecycle(t, tenantHost, token, orgID, wsID, "/pause")
		if status != http.StatusOK {
			t.Fatalf("pause: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "paused" {
			t.Fatalf("pause: body status=%q (expected paused): %s", st, body)
		}
		waitForWorkspaceStatus(t, tenantHost, token, orgID, wsID, "paused", 3*time.Minute, "pause→paused")
		// Genuinely-stopped assertion: the canvas GET clears url on pause
		// (Pause SETs url=''), and a stopped container no longer serves A2A.
		// A handler that only flipped a flag without stopping the container
		// would still be reachable here — so this is the real-stop signal.
		assertURLCleared(t, tenantHost, token, orgID, wsID, 3*time.Minute, "pause")
		assertNotServing(t, tenantHost, token, orgID, wsID, "pause")
		t.Logf("pause VERIFIED: paused + url cleared + container unserveable (genuinely stopped)")

		// resume → provisioning → online + serveable again.
		status, body = postLifecycle(t, tenantHost, token, orgID, wsID, "/resume")
		if status != http.StatusOK {
			t.Fatalf("resume: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "provisioning" {
			t.Fatalf("resume: body status=%q (expected provisioning): %s", st, body)
		}
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "resume→online")
		assertServes(t, tenantHost, token, orgID, wsID, "post-resume")
		t.Logf("resume VERIFIED: paused → provisioning → online + serveable")
	})

	// ── hibernate → wake ────────────────────────────────────────────────────
	t.Run("hibernate_wake", func(t *testing.T) {
		// hibernate (force, since a fresh online ws may carry no active tasks
		// but we don't want a transient active_tasks>0 to 409 the test).
		status, body := postLifecycle(t, tenantHost, token, orgID, wsID, "/hibernate?force=true")
		if status != http.StatusOK {
			t.Fatalf("hibernate: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "hibernated" {
			t.Fatalf("hibernate: body status=%q (expected hibernated): %s", st, body)
		}
		// Confirm it settled at 'hibernated' (not stuck mid-'hibernating') and
		// the container is genuinely stopped (url cleared + unserveable).
		waitForWorkspaceStatus(t, tenantHost, token, orgID, wsID, "hibernated", 3*time.Minute, "hibernate→hibernated")
		assertURLCleared(t, tenantHost, token, orgID, wsID, 3*time.Minute, "hibernate")
		assertNotServing(t, tenantHost, token, orgID, wsID, "hibernate")
		t.Logf("hibernate VERIFIED: hibernated + url cleared + container unserveable")

		// wake: a hibernated workspace auto-wakes on the next incoming A2A
		// message (NOT /resume — Resume only handles status=paused). The wake
		// A2A itself may return transient 5xx while the container re-provisions;
		// the load-bearing contract is the STATUS transition back to online.
		sendWakeA2A(t, tenantHost, token, orgID, wsID)
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "hibernate→wake→online")
		assertServes(t, tenantHost, token, orgID, wsID, "post-wake")
		t.Logf("wake VERIFIED: hibernated → online via auto-wake A2A + serveable")
	})
}

// ---------------------------------------------------------------------------
// lifecycle drivers + observable-state assertions
// ---------------------------------------------------------------------------

// postLifecycle POSTs a lifecycle endpoint (path includes any ?query) on the
// tenant ws-server using the tenant admin token (accepted by WorkspaceAuth).
func postLifecycle(t *testing.T, host, token, orgID, wsID, pathAndQuery string) (int, string) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID + pathAndQuery
	return doTenantJSON(t, "POST", url, token, orgID, "")
}

// workspaceStatusAndURL reads the canvas GET /workspaces/:id and returns
// (status, url). url is "" when the workspace is not routable (paused/hibernated
// clear it). httpStatus is surfaced so callers can distinguish 404/Gone.
func workspaceStatusAndURL(t *testing.T, host, token, orgID, wsID string) (httpStatus int, status, url string) {
	t.Helper()
	u := "https://" + host + "/workspaces/" + wsID
	hs, body := doTenantJSON(t, "GET", u, token, orgID, "")
	return hs, jsonField(body, "status"), jsonField(body, "url")
}

// waitForWorkspaceStatus polls the canvas GET until .status == want.
func waitForWorkspaceStatus(t *testing.T, host, token, orgID, wsID, want string, timeout time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		_, st, _ := workspaceStatusAndURL(t, host, token, orgID, wsID)
		if st != last {
			t.Logf("    [%s] status → %q", why, st)
			last = st
		}
		if st == want {
			return
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("%s: workspace %s never reached status=%q within %s (last=%q)", why, wsID, want, timeout, last)
}

// waitForWorkspaceOnlineRoutable polls until status=online AND url is non-empty.
// A routable url is the real "the agent is reachable" signal the SDK uses — an
// online row without a url is not yet serveable.
func waitForWorkspaceOnlineRoutable(t *testing.T, host, token, orgID, wsID string, timeout time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus, lastURL string
	for time.Now().Before(deadline) {
		_, st, url := workspaceStatusAndURL(t, host, token, orgID, wsID)
		if st != lastStatus || (url != "") != (lastURL != "") {
			t.Logf("    [%s] status=%q routable=%v", why, st, url != "")
			lastStatus, lastURL = st, url
		}
		if st == "online" && url != "" {
			return
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("%s: workspace %s never reached online+routable within %s (last status=%q, url-set=%v)",
		why, wsID, timeout, lastStatus, lastURL != "")
}

// assertURLCleared asserts the canvas GET reports an empty url within timeout.
// Pause/Hibernate SET url=” as part of stopping the container; a non-empty url
// means the workspace is still routable (container not stopped).
func assertURLCleared(t *testing.T, host, token, orgID, wsID string, timeout time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastURL string
	for time.Now().Before(deadline) {
		_, _, url := workspaceStatusAndURL(t, host, token, orgID, wsID)
		lastURL = url
		if url == "" {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("%s: workspace %s url never cleared within %s (last url-set=%v) — container may not have actually stopped",
		why, wsID, timeout, lastURL != "")
}

// serveProbe sends one A2A message/send to the workspace and reports whether the
// agent served it (2xx). A 2xx means a live container handled the request; a
// connection error / 5xx / 4xx means it did not serve.
func serveProbe(t *testing.T, host, token, orgID, wsID string) (served bool, code int) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID + "/a2a"
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"message/send","id":"e2e-probe","params":{"message":{"role":"user","messageId":%q,"parts":[{"kind":"text","text":"platform lifecycle e2e serve probe — reply with the single token: PONG"}]}}}`,
		fmt.Sprintf("e2e-probe-%d", time.Now().UnixNano()))
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build serve probe: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Molecule-Org-Id", orgID)
	req.Header.Set("Origin", "https://"+host)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	drain(resp)
	return resp.StatusCode >= 200 && resp.StatusCode < 300, resp.StatusCode
}

// assertServes requires the workspace to serve an A2A round-trip within a short
// readiness window (it may have just transitioned to online; allow brief warmup
// + tolerate transient cold 5xx, same edge class the shell harness tolerates).
func assertServes(t *testing.T, host, token, orgID, wsID, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	var lastCode int
	for time.Now().Before(deadline) {
		served, code := serveProbe(t, host, token, orgID, wsID)
		lastCode = code
		if served {
			return
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("%s: workspace %s never served an A2A round-trip within 5m (last http=%d) — online but not serveable",
		why, wsID, lastCode)
}

// assertNotServing requires the workspace to STOP serving A2A within timeout —
// the observable proxy (via the tenant API, no AWS/SSM access in core) that the
// container is genuinely stopped, not merely flagged paused/hibernated.
//
// NOTE: a hibernated workspace auto-wakes on the NEXT A2A message — so a single
// probe could itself trigger a wake. We therefore look for the workspace to be
// unreachable on the FIRST probe taken after the status/url already settled to
// stopped; we do not retry-poll the probe (that would wake it). A live-and-
// serving container returns 2xx immediately, which is the regression we catch.
//
// TODO(core#2332): the strongest "container stopped" signal is the EC2/Docker
// state itself (instance stopped), which is only observable from the CP side
// (AWS/SSM) — not reachable from the core ws-server module without importing the
// CP client surface. This asserts the strongest signal available here (url
// cleared + immediate non-serve). If/when a CP-side admin endpoint surfaces the
// instance power-state to the tenant API, tighten this to assert it directly.
func assertNotServing(t *testing.T, host, token, orgID, wsID string, why string) {
	t.Helper()
	// The status/url already settled to stopped before this is called. One
	// probe — not a retry loop — to avoid auto-waking a hibernated workspace.
	served, code := serveProbe(t, host, token, orgID, wsID)
	if served {
		t.Fatalf("%s: workspace %s STILL serves A2A (http=%d) after status settled to stopped — "+
			"container was not actually stopped (handler flipped the flag only)", why, wsID, code)
	}
	t.Logf("    [%s] workspace unserveable after stop (probe http=%d) — container genuinely stopped", why, code)
}

// sendWakeA2A sends a wake message to a hibernated workspace. The wake A2A may
// itself return transient 5xx while the container re-provisions — we send it
// best-effort with bounded retries on the cold-restart 5xx class and let the
// caller assert the real contract (status → online).
func sendWakeA2A(t *testing.T, host, token, orgID, wsID string) {
	t.Helper()
	for attempt := 1; attempt <= 12; attempt++ {
		served, code := serveProbe(t, host, token, orgID, wsID)
		if served {
			t.Logf("    wake A2A served (http=%d) on attempt %d", code, attempt)
			return
		}
		// 5xx / 0 (conn refused while container is down) are expected during
		// cold wake — retry. The wake has still been dispatched (it reaches the
		// ProxyA2A handler, which triggers re-provision); we just couldn't get a
		// 2xx synchronously. Keep nudging until the status assertion takes over.
		t.Logf("    wake A2A attempt %d/12: http=%d (cold restart) — retrying", attempt, code)
		time.Sleep(15 * time.Second)
	}
	t.Logf("    wake A2A did not return 2xx within retries — relying on status→online assertion to confirm wake")
}

// drain reads and discards a response body (cap 1 MiB) so the connection can be
// reused / closed cleanly.
func drain(resp *http.Response) {
	buf := make([]byte, 4096)
	total := 0
	for {
		n, e := resp.Body.Read(buf)
		total += n
		if e != nil || total > 1<<20 {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// harness (self-contained — this package is excluded from the default build).
// Mirrors the idioms of cp's internal/staginge2e (cp#386): STAGING_E2E=1 gate,
// CP_ADMIN_API_TOKEN admin surface, provision→wait-online→assert, t.Cleanup
// teardown. Core has no CP client packages, so these are HTTP-only.
// ---------------------------------------------------------------------------

type stagingCfg struct {
	cpBase          string
	adminToken      string
	subdomainSuffix string
}

// requireStagingEnv gates the suite. STAGING_E2E != 1 SKIPs (the suite's
// contract — advisory-by-infra, not fail-open within a run). With STAGING_E2E=1
// but creds absent it also skips LOUD (so a misconfigured CI run can't false-
// green by silently passing zero assertions).
func requireStagingEnv(t *testing.T) stagingCfg {
	t.Helper()
	if os.Getenv("STAGING_E2E") != "1" {
		t.Skip("STAGING_E2E != 1 — skipping live staging e2e (set STAGING_E2E=1 + CP_BASE_URL + CP_ADMIN_API_TOKEN to run)")
	}
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	cfg := stagingCfg{
		cpBase:          strings.TrimRight(get("CP_BASE_URL"), "/"),
		adminToken:      get("CP_ADMIN_API_TOKEN"),
		subdomainSuffix: envOr("STAGING_TENANT_SUBDOMAIN_SUFFIX", "staging.moleculesai.app"),
	}
	var missing []string
	for k, v := range map[string]string{
		"CP_BASE_URL":        cfg.cpBase,
		"CP_ADMIN_API_TOKEN": cfg.adminToken,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("STAGING_E2E=1 but missing required env: %s — skipping LOUD (not a silent pass)", strings.Join(missing, ", "))
	}
	return cfg
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// adminCreateOrg provisions a throwaway org via the CP admin API and waits for
// its instance to reach running (provisioning is async).
func adminCreateOrg(t *testing.T, cfg stagingCfg, slug string) (orgID string) {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"name":%q,"owner_user_id":%q}`, slug, "E2E Workspace Lifecycle", "e2e-runner:"+slug)
	status, resp := doJSON(t, "POST", cfg.cpBase+"/cp/admin/orgs", cfg.adminToken, body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("AdminCreate org: HTTP %d: %s", status, resp)
	}
	id := jsonField(resp, "id")
	if id == "" {
		t.Fatalf("AdminCreate org: no id in response: %s", resp)
	}
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		st, list := doJSON(t, "GET", cfg.cpBase+"/cp/admin/orgs", cfg.adminToken, "")
		if st == http.StatusOK && strings.Contains(list, `"slug":"`+slug+`"`) &&
			orgInstanceStatus(list, slug) == "running" {
			return id
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("org %s did not reach instance_status=running within timeout", slug)
	return ""
}

func adminDeleteTenant(t *testing.T, cfg stagingCfg, slug string) {
	t.Helper()
	body := fmt.Sprintf(`{"confirm":%q}`, slug)
	status, resp := doJSON(t, "DELETE", cfg.cpBase+"/cp/admin/tenants/"+slug, cfg.adminToken, body)
	if status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNotFound {
		t.Logf("WARNING: teardown DELETE tenant %s returned HTTP %d: %s (manual cleanup may be needed)", slug, status, resp)
		return
	}
	t.Logf("teardown: deleted tenant %s (HTTP %d)", slug, status)
}

// tenantAdminToken fetches the per-tenant admin token from the CP admin surface.
// Only available once the tenant platform has finished provisioning.
func tenantAdminToken(t *testing.T, cfg stagingCfg, slug string) string {
	t.Helper()
	url := cfg.cpBase + "/cp/admin/orgs/" + slug + "/admin-token"
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		status, body := doJSON(t, "GET", url, cfg.adminToken, "")
		if status == http.StatusOK {
			if tok := jsonField(body, "admin_token"); tok != "" {
				return tok
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("tenant admin token not available for %s within timeout", slug)
	return ""
}

// tenantCreateWorkspace creates a default claude-code workspace via the tenant
// ws-server, exercising the full tenant → CP provisioner → EC2 path.
func tenantCreateWorkspace(t *testing.T, cfg stagingCfg, host, token, orgID string) string {
	t.Helper()
	return tenantCreateWorkspaceWithRuntime(t, cfg, host, token, orgID, "core2332-life-e2e", "claude-code", "moonshot/kimi-k2.6")
}

// tenantCreateWorkspaceWithRuntime creates a workspace on a specific runtime +
// model. Both must be a PLATFORM-namespaced pairing so billing resolves
// platform_managed (no BYOK credential needed at create) — used by the
// external-runtime request e2e (codex + the cheap openai/gpt-5.4-mini).
func tenantCreateWorkspaceWithRuntime(t *testing.T, cfg stagingCfg, host, token, orgID, name, runtime, model string) string {
	t.Helper()
	url := "https://" + host + "/workspaces"
	body := fmt.Sprintf(
		`{"name":%q,"runtime":%q,"tier":%d,"model":%q,"billing_mode":%q,"provider":%q}`,
		name, runtime, 1, model, "platform_managed", "platform",
	)
	status, resp := doTenantJSON(t, "POST", url, token, orgID, body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("tenant workspace create (runtime=%s model=%s): HTTP %d: %s", runtime, model, status, resp)
	}
	id := jsonField(resp, "id")
	if id == "" {
		t.Fatalf("tenant workspace create: no id in response: %s", resp)
	}
	return id
}

// --- reachability ----------------------------------------------------------

func waitForHTTP(t *testing.T, host string, want int, timeout time.Duration, why string) {
	t.Helper()
	url := "https://" + host + "/health"
	client := &http.Client{Timeout: 15 * time.Second}
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", url, nil)
		resp, err := client.Do(req)
		if err == nil {
			last = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("%s: %s never returned HTTP %d within %s (last=%d)", why, url, want, timeout, last)
}

// --- HTTP helpers ----------------------------------------------------------

// doJSON hits the CP admin surface (bearer admin token, no tenant headers).
func doJSON(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 150 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp)
}

// doTenantJSON hits the tenant ws-server. It adds the three headers the SaaS
// auth chain requires: Authorization (tenant admin token), X-Molecule-Org-Id
// (tenant guard 404s anything without it), and Origin (Cloudflare WAF rejects a
// mismatched/absent Origin with 404).
func doTenantJSON(t *testing.T, method, url, token, orgID, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Molecule-Org-Id", orgID)
	req.Header.Set("Origin", "https://"+strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)[0])
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp)
}

func readBody(resp *http.Response) string {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if e != nil || len(buf) > 1<<20 {
			break
		}
	}
	return string(buf)
}

// jsonField does a flat, dependency-free extraction of a top-level string field
// value ("key":"value") — sufficient for the id/status/url fields we read.
func jsonField(body, key string) string {
	needle := `"` + key + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// orgInstanceStatus finds the instance_status for a given slug in a
// /cp/admin/orgs list response by scanning the object that contains the slug.
func orgInstanceStatus(listBody, slug string) string {
	marker := `"slug":"` + slug + `"`
	i := strings.Index(listBody, marker)
	if i < 0 {
		return ""
	}
	lo := i - 600
	if lo < 0 {
		lo = 0
	}
	hi := i + 600
	if hi > len(listBody) {
		hi = len(listBody)
	}
	return jsonField(listBody[lo:hi], "instance_status")
}
