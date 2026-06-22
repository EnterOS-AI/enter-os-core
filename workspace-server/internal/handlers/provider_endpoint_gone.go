package handlers

// internal#718 P4 closure — provider endpoint retirement.
//
// PUT and GET /workspaces/:id/provider were the canvas-facing surface
// for the legacy `LLM_PROVIDER` workspace_secret. With the registry-
// derived provider model (P0-P4), the provider is now DERIVED at every
// decision point from (runtime, model) via the registry. No code path
// reads a stored provider anymore, so the endpoint has no observable
// effect.
//
// Rather than silently 200-OK on a write that goes nowhere, the
// retired endpoint returns 410 Gone with a structured body so an
// older canvas (which still calls PUT /provider in its Save flow)
// surfaces a loud-and-clear "this endpoint moved" error rather than
// pretending to persist a change. The replacement is: select your
// model on workspace create / via PUT /workspaces/:id/model — the
// provider is derived from it.
//
// Retirement context:
//   - Retire-list #2 (CP `knownProviderNames` blocklist as authoring
//     surface) was already retired in P3 PR-C (cp#379) — that source
//     now reads from the registry. The CP-side reader of
//     `env["LLM_PROVIDER"]` (`resolveModelAndProvider`) is replaced in
//     the CP-side commit of this PR by a registry derivation.
//   - Retire-list #3 (`deriveProviderFromModelSlug`) is removed in
//     this PR — the only caller was `WorkspaceHandler.Create`, which
//     wrote the derived value into workspace_secrets.LLM_PROVIDER for
//     the now-removed CP read path. The migration 20260528000000
//     deletes any straggler rows from the secret table.
//
// The Gone body is the contract: callers must recognize
// `code: PROVIDER_ENDPOINT_RETIRED` and stop calling. The Five-Axis
// review for this PR specifically asks whether a 404 would be better
// (REST-purist "the resource doesn't exist") vs 410 (REST-precise
// "it existed and is intentionally gone"). 410 is correct here: the
// endpoint shipped to prod, the canvas knows the URL, and the goal
// is to make the retirement loud, not invisible.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ProviderEndpointGone is the replacement gin handler for GET/PUT
// /workspaces/:id/provider. Returns 410 with a body shape the canvas
// can pattern-match on (code/error/issue keys).
//
// Wired in internal/router/router.go (the two route lines that used
// to reference sech.GetProvider / sech.SetProvider).
//
// Exported so the router package can reference it as
// handlers.ProviderEndpointGone without spinning up a SecretsHandler
// receiver just to retire two endpoints.
func ProviderEndpointGone(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{
		"code":  "PROVIDER_ENDPOINT_RETIRED",
		"error": "the LLM_PROVIDER workspace_secret has been retired; the provider is now derived from (runtime, model) via the registry. Select your model via PUT /workspaces/:id/model — the provider follows.",
		"issue": "internal#718",
	})
}
