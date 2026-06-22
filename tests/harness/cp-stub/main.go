// cp-stub — minimal control-plane stand-in for the local production-shape harness.
//
// In production, the tenant Go server reverse-proxies /cp/* to the SaaS
// control-plane (molecule-controlplane). This stub plays that role on
// localhost so we can exercise the SAME code path the tenant takes in
// production — `if cpURL := os.Getenv("CP_UPSTREAM_URL"); cpURL != ""`
// in workspace-server/internal/router/router.go fires, the proxy mount
// activates, and tests exercise the real tenant→CP wire.
//
// This is NOT a CP reimplementation. It serves the minimum surface to:
//  1. Boot the tenant image without /cp/* breaking the canvas bootstrap.
//  2. Replay specific bug classes (e.g. /cp/* returns 404, returns 5xx,
//     returns malformed JSON) by toggling env vars.
//
// Scope is bounded by what the tenant + canvas actually call. Add new
// handlers as new replay scenarios demand them. Drift from real CP is
// tolerated because each handler is named for the exact path it serves —
// when the real CP changes, the failing scenario tells us where to look.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

// redeployFleetCalls tracks how many times /cp/admin/tenants/redeploy-fleet
// was invoked. Replay scripts assert > 0 to confirm the workflow's redeploy
// step actually reached the stub (catches misrouted CP_URL configs).
var redeployFleetCalls atomic.Int64

// provisionCalls tracks how many times /cp/workspaces/provision was
// invoked. Phase 1 of the #2863 burn-down: a green-counter for the
// cp-stub-provision-config replay that proves the harness provision
// call actually reached the stub (and didn't fly past to real prod CP
// via the env-var mismatch on CP_UPSTREAM_URL vs CP_PROVISION_URL).
var provisionCalls atomic.Int64

// tenantsConfigCalls tracks how many times /cp/tenants/config was
// invoked. Companion counter for the same Phase 1 burn-down — proves
// the harness config-fetch also reached the stub.
var tenantsConfigCalls atomic.Int64

func main() {
	mux := http.NewServeMux()

	// /cp/auth/me — canvas calls this on bootstrap; minimal user record
	// keeps the canvas from redirecting to login during local E2E.
	mux.HandleFunc("/cp/auth/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"id":     "harness-user",
			"email":  "harness@local",
			"org_id": "harness-org",
			"roles":  []string{"admin"},
		})
	})

	// /cp/admin/tenants/redeploy-fleet — exercised by the
	// redeploy-tenants-on-{staging,main} workflow's local replay. Returns
	// the same shape the real CP returns so the verify-fleet logic in CI
	// can be tested without spinning up a real EC2 fleet.
	mux.HandleFunc("/cp/admin/tenants/redeploy-fleet", func(w http.ResponseWriter, r *http.Request) {
		redeployFleetCalls.Add(1)
		writeJSON(w, 200, map[string]any{
			"ok": true,
			"results": []map[string]any{
				{
					"slug":          "harness-tenant",
					"phase":         "redeploy",
					"ssm_status":    "Success",
					"ssm_exit_code": 0,
					"healthz_ok":    true,
				},
			},
		})
	})

	// /cp/admin/orgs — POST. Mirrors the real CP's orgs.go:267-295 +
	// router.go:437 validation shape: org-create requires slug, name,
	// and owner_user_id. The harness's canary-smoke-org-create-400-capture
	// replay (tests/harness/replays/) posts a payload missing
	// owner_user_id and asserts the stub returns 400 + a parseable JSON
	// body naming the missing fields. This is the harness-capture path
	// for the real core#2737 staging 400-body-loss (the staging script
	// eats the body under set -e + admin_call; the harness proves the
	// pattern works locally).
	//
	// Burn-down for #2864: registering this handler un-arms the
	// canary-smoke-org-create-400-capture xfail.
	mux.HandleFunc("/cp/admin/orgs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{"error": "method not allowed; expected POST"})
			return
		}
		var payload struct {
			Slug        string `json:"slug"`
			Name        string `json:"name"`
			OwnerUserID string `json:"owner_user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, 400, map[string]any{"error": "invalid JSON: " + err.Error()})
			return
		}
		var missing []string
		if payload.Slug == "" {
			missing = append(missing, "slug")
		}
		if payload.Name == "" {
			missing = append(missing, "name")
		}
		if payload.OwnerUserID == "" {
			missing = append(missing, "owner_user_id")
		}
		if len(missing) > 0 {
			writeJSON(w, 400, map[string]any{
				"error":  strings.Join(missing, ", ") + " are required",
				"fields": missing,
			})
			return
		}
		// If the payload is valid, return 201 (real CP behavior). The
		// replay doesn't exercise this path — it specifically tests the
		// 400 + body shape on a bad payload — but returning 201 keeps
		// the stub honest for any future replay that wants to test
		// the happy path.
		writeJSON(w, 201, map[string]any{
			"ok":   true,
			"slug": payload.Slug,
		})
	})

	// /cp/workspaces/provision — Phase 1 of the #2863 burn-down. The
	// real CP returns 201 + a provision-response shape that the tenant
	// Go code (workspace-server's CPProvisioner.Start in
	// internal/provisioner/cp_provisioner.go:339-363) treats as
	// success. That client (the cpProvisionResponse struct) reads
	// exactly two fields on success: instance_id + state. The
	// cp-stub mirrors that contract — 201 + those two fields — so
	// the harness-tenant Go code (which uses the REAL
	// CPProvisioner client) treats the response as a successful
	// provision. Anything else and the client falls into its
	// failure branch with `provision failed (200): <unstructured
	// body>` (the exact failure mode the CR2 review_id 11928
	// flagged on the prior head 30a6bea: 200 instead of 201, no
	// instance_id/state fields, → guaranteed fail-branch).
	//
	// cp-stub is permissive on input (no auth header check, empty
	// body OK, no payload-field validation) — the call's purpose is
	// to PROVE the request reached the stub + the env-var redirect
	// is wired. Field validation lives in the real CP in production.
	mux.HandleFunc("/cp/workspaces/provision", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{
				"error": "cp-stub: /cp/workspaces/provision only accepts POST",
			})
			return
		}
		provisionCalls.Add(1)
		// Parse body for shape (default to harness-ws if empty)
		wsID := "harness-ws"
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			if json.Unmarshal(body, &payload) == nil {
				if v, ok := payload["workspace_id"].(string); ok && v != "" {
					wsID = v
				}
			}
		}
		// Stub instance id + state — matches the real CP's success-path
		// contract. EC2 instance ids start with "i-" (the real CP
		// generates them via EC2 RunInstances; the stub is a stand-in,
		// but the prefix keeps any future real-CP log-reader from
		// false-flagging the stub response as malformed). "running"
		// matches the prod happy path; the harness doesn't await
		// any state transition.
		instanceID := "i-stub-" + wsID
		state := "running"
		log.Printf("cp-stub: /cp/workspaces/provision called (count=%d) -> %s (instance_id=%s, state=%s)", provisionCalls.Load(), wsID, instanceID, state)
		writeJSON(w, 201, map[string]any{
			// Fields the tenant Go code reads (cpProvisionResponse
			// struct in internal/provisioner/cp_provisioner.go:210-215):
			// instance_id (string) + state (string). Mandatory.
			"instance_id": instanceID,
			"state":       state,
			// Observability fields — the real CP returns these too
			// (the real CPProvisioner.client ignores them, but they
			// appear in the wire log + in any future tool that
			// inspects the response). Mirror the prior head's
			// payload shape for minimum drift from the 30a6bea
			// contract.
			"workspace_id": wsID,
			"url":          "http://cp-stub:9090/cp/workspaces/" + wsID,
		})
	})

	// /cp/tenants/config — companion handler for Phase 1 of the #2863
	// burn-down. Mirrors the real CP's tenant-config response shape
	// (cp_config.go:47-63 in molecule-core): returns the runtime
	// registry, LLM endpoints, and feature flags a tenant needs to
	// bootstrap. The stub returns a minimal but valid config — enough
	// for the harness tenant to complete its boot sequence without
	// falling through to a real CP call.
	mux.HandleFunc("/cp/tenants/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, 405, map[string]any{
				"error": "cp-stub: /cp/tenants/config only accepts GET",
			})
			return
		}
		tenantsConfigCalls.Add(1)
		log.Printf("cp-stub: /cp/tenants/config called (count=%d)", tenantsConfigCalls.Load())
		writeJSON(w, 200, map[string]any{
			"tenant_id": "harness-tenant",
			"runtimes": []string{
				"claude-code",
				"hermes",
				"openclaw",
				"codex",
				"google-adk",
				"seo-agent",
			},
			"llm_endpoints": map[string]string{
				"openai":    "http://cp-stub:9090/llm/openai/v1",
				"anthropic": "http://cp-stub:9090/llm/anthropic/v1",
			},
			"feature_flags": map[string]bool{
				"canvas_async_dispatch":   true,
				"runtime_provision_smoke": true,
				"secrets_encryption_key":  true,
			},
		})
	})

	// __stub/state — expose stub state (counters) so replay scripts can
	// assert the tenant actually reached us. Read-only.
	mux.HandleFunc("/__stub/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"redeploy_fleet_calls": redeployFleetCalls.Load(),
			"provision_calls":      provisionCalls.Load(),
			"tenants_config_calls": tenantsConfigCalls.Load(),
		})
	})

	// Catch-all for any /cp/* the tenant proxies. Keeps the harness from
	// crashing the canvas when a new CP route is added — surfaces a clear
	// "stub doesn't implement X" error instead of opaque 502 from the
	// reverse proxy.
	mux.HandleFunc("/cp/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 501, map[string]any{
			"error": "cp-stub: handler not implemented for " + r.Method + " " + r.URL.Path,
			"hint":  "add a handler in tests/harness/cp-stub/main.go for the scenario you're testing",
		})
	})

	// /healthz — readiness probe for compose's depends_on.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok"})
	})

	addr := ":" + envOr("PORT", "9090")
	log.Printf("cp-stub listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		fmt.Fprintf(os.Stderr, "cp-stub: write json: %v\n", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
