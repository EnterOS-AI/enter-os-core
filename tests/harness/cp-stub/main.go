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
//   1. Boot the tenant image without /cp/* breaking the canvas bootstrap.
//   2. Replay specific bug classes (e.g. /cp/* returns 404, returns 5xx,
//      returns malformed JSON) by toggling env vars.
//
// Scope is bounded by what the tenant + canvas actually call. Add new
// handlers as new replay scenarios demand them. Drift from real CP is
// tolerated because each handler is named for the exact path it serves —
// when the real CP changes, the failing scenario tells us where to look.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
)

// redeployFleetCalls tracks how many times /cp/admin/tenants/redeploy-fleet
// was invoked. Replay scripts assert > 0 to confirm the workflow's redeploy
// step actually reached the stub (catches misrouted CP_URL configs).
var redeployFleetCalls atomic.Int64

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

	// __stub/state — expose stub state (counters) so replay scripts can
	// assert the tenant actually reached us. Read-only.
	mux.HandleFunc("/__stub/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"redeploy_fleet_calls": redeployFleetCalls.Load(),
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
