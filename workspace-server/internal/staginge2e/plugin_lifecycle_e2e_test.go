//go:build staging_e2e

package staginge2e

// plugin_lifecycle_e2e_test.go — live, against-real-staging guard for the
// tenant plugin-install lifecycle that backs the canvas Plugins tab.
//
// Guards three regressions that were each invisible to the existing suites:
//
//   1. Registry is non-empty (the canvas "no registry available" /
//      "Registry returned 0 plugins" report): GET /plugins on the tenant
//      ws-server must list the plugins clone-manifest.sh populates at deploy
//      time. A 0-length registry here is the exact server-side state that
//      makes the install dialog look broken.
//
//   2. ListInstalled returns an installed plugin on a SaaS workspace
//      (guards CP #3125 — the EIC branch that fixed "[] readback after a
//      successful install" for every SaaS tenant): install a registry plugin
//      via POST /workspaces/:id/plugins, then GET /workspaces/:id/plugins must
//      include it. Without the EIC branch this read back [] forever.
//
//   3. The agent stays online after the install-triggered restart (guards
//      the #159 mgmt-MCP self-heal — a plugin install restarts the workspace,
//      and a broken online-gate would leave it stuck 'failed'/offline): the
//      workspace must return to online+routable AND serve A2A after the
//      install.
//
// Reuses the workspace_lifecycle_test.go harness wholesale (requireStagingEnv
// / adminCreateOrg / adminDeleteTenant / tenantAdminToken / tenantCreateWorkspace
// / waitForWorkspaceOnlineRoutable / waitForWorkspaceStatus / doTenantJSON /
// serveProbe / jsonField). NOTHING here re-implements org provisioning or
// teardown — t.Cleanup-driven admin DELETE guarantees no leaked tenant.
//
// Guarded by the staging_e2e build tag + STAGING_E2E=1 (requireStagingEnv).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestPluginInstallLifecycle_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := fmt.Sprintf("e2e-plgn-%d", time.Now().Unix()%100000000)
	t.Logf("plugin-lifecycle: slug=%s", slug)

	// --- Step 1: provision throwaway org + tenant (reused scaffolding) ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	host := slug + "." + cfg.subdomainSuffix
	token := tenantAdminToken(t, cfg, slug)
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)

	// --- Step 2: registry must be non-empty BEFORE we pick a plugin ----------
	// This is independent of any workspace — the registry is host-local
	// (clone-manifest.sh populated). We both assert it's non-empty (guard #1)
	// AND use the first runtime-agnostic entry as the plugin to install, so the
	// test never hardcodes a registry slug that could be renamed/removed.
	var pluginName string
	t.Run("registry_non_empty", func(t *testing.T) {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/plugins", token, orgID, "")
		if hs != http.StatusOK {
			t.Fatalf("GET /plugins registry: HTTP %d: %s", hs, body)
		}
		reg := parsePluginList(t, body)
		if len(reg) == 0 {
			t.Fatalf("GET /plugins returned an EMPTY registry — the canvas install dialog would "+
				"show 'no registry available'. clone-manifest.sh must populate plugins/ at deploy. body=%s",
				truncate(body, 400))
		}
		pluginName = pickInstallablePlugin(reg)
		if pluginName == "" {
			t.Fatalf("registry has %d entries but none with a usable name: %s", len(reg), truncate(body, 400))
		}
		t.Logf("registry has %d plugins; will install %q", len(reg), pluginName)
	})
	if pluginName == "" {
		t.Fatal("no installable plugin established by registry_non_empty — dependent subtests cannot run")
	}

	// --- Step 3: create + boot a workspace ----------------------------------
	wsID := tenantCreateWorkspace(t, cfg, host, token, orgID)
	t.Logf("workspace created: %s", wsID)
	waitForWorkspaceOnlineRoutable(t, host, token, orgID, wsID, 15*time.Minute, "initial boot")
	t.Logf("workspace %s online + routable", wsID)

	// --- Step 4: install → ListInstalled returns it → agent stays online -----
	t.Run("install_then_list_then_stay_online", func(t *testing.T) {
		// Install the registry plugin via the local:// source — the same path
		// the canvas "Install" button uses (handleInstall → local://<name>).
		installBody, _ := json.Marshal(map[string]string{"source": "local://" + pluginName})
		hs, body := doTenantJSON(t, "POST", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, string(installBody))
		if hs != http.StatusOK && hs != http.StatusCreated && hs != http.StatusAccepted {
			t.Fatalf("install %q: HTTP %d: %s", pluginName, hs, body)
		}
		t.Logf("install %q accepted (HTTP %d) — workspace will restart", pluginName, hs)

		// The install restarts the workspace. Wait for it to come back
		// online+routable. A stuck 'failed'/offline here is the #159 self-heal
		// regression (online-gate refusing a freshly-rebooted box).
		waitForWorkspaceOnlineRoutable(t, host, token, orgID, wsID, 15*time.Minute, "post-install restart→online")

		// ListInstalled must now include the plugin (guard #3125 EIC branch).
		// The readback can race the restart, so poll until it appears.
		if !pollListInstalledContains(t, host, token, orgID, wsID, pluginName, 5*time.Minute) {
			_, listBody := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, "")
			t.Fatalf("installed plugin %q never appeared in ListInstalled within 5m — "+
				"the SaaS EIC readback (#3125) regressed; last list=%s", pluginName, truncate(listBody, 600))
		}
		t.Logf("ListInstalled returned %q after install (EIC readback OK)", pluginName)

		// And the agent must actually SERVE — online-row + a live A2A reply.
		if served, code := serveProbe(t, host, token, orgID, wsID); !served {
			t.Fatalf("agent did not serve A2A after plugin install (code=%d) — "+
				"the install-triggered restart left it un-serveable (#159 self-heal regression)", code)
		}
		t.Logf("agent served A2A after install — stayed online through the restart")
	})
}

// ─── helpers (plugin-lifecycle specific; lifecycle suite owns the shared ones) ──

// pluginListRow is the flat view of one /plugins or /workspaces/:id/plugins row.
type pluginListRow struct {
	Name    string
	Version string
}

func parsePluginList(t *testing.T, body string) []pluginListRow {
	t.Helper()
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("plugins body not a JSON array: %v (%s)", err, truncate(body, 300))
	}
	out := make([]pluginListRow, 0, len(raw))
	for _, m := range raw {
		out = append(out, pluginListRow{
			Name:    rawString(m["name"]),
			Version: rawString(m["version"]),
		})
	}
	return out
}

// pickInstallablePlugin returns the first registry entry with a non-empty name.
// (Runtime filtering is handled server-side; an unnamed entry is unusable.)
func pickInstallablePlugin(reg []pluginListRow) string {
	for _, p := range reg {
		if p.Name != "" {
			return p.Name
		}
	}
	return ""
}

// pollListInstalledContains polls GET /workspaces/:id/plugins until name appears
// or the timeout elapses. Returns true if found.
func pollListInstalledContains(t *testing.T, host, token, orgID, wsID, name string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+wsID+"/plugins", token, orgID, "")
		if hs == http.StatusOK {
			for _, p := range parsePluginList(t, body) {
				if p.Name == name {
					return true
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	return false
}
