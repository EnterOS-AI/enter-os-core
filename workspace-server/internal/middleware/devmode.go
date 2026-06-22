package middleware

import (
	"os"
	"strings"
)

// Local-dev environment detection.
//
// SECURITY (harden/no-fail-open-auth): this file used to export an auth
// escape hatch — `isDevModeFailOpen()` — that let AdminAuth, WorkspaceAuth,
// and the discovery handler serve admin/workspace-protected endpoints with
// NO bearer token whenever `ADMIN_TOKEN` was unset and `MOLECULE_ENV` was a
// dev value. The CTO directive is "nothing should be fail-open": auth is now
// fail-CLOSED in every environment, dev included. The hatch is GONE.
//
// What remains here is a NON-security predicate, `isLocalDevEnv()`, that
// reports ONLY whether `MOLECULE_ENV` names a local-dev environment. It does
// NOT consult `ADMIN_TOKEN` and it does NOT influence authentication. It is
// used for two convenience/defense-in-depth knobs that never grant access:
//
//   - ratelimit.go: relax the per-caller request bucket on a single-user
//     local stack (a DoS knob, not a credential — relaxing it cannot expose
//     any protected data).
//   - cmd/server resolveBindHost(): default the HTTP listener to loopback
//     (127.0.0.1) in local dev. This is strictly *safer* than binding all
//     interfaces and is unrelated to whether a request is authenticated.
//
// Local dev now stays AUTHENTICATED, not open: scripts/dev-start.sh
// provisions a deterministic `ADMIN_TOKEN` and hands the matching
// `NEXT_PUBLIC_ADMIN_TOKEN` to the Canvas, so the browser sends a real
// bearer. See scripts/dev-start.sh and canvas/src/lib/api.ts.

// devModeEnvValues is the set of MOLECULE_ENV values that count as
// "explicit local dev". Production callers don't set any of these.
// Case-insensitive compare via strings.ToLower below.
var devModeEnvValues = map[string]struct{}{
	"development": {},
	"dev":         {},
}

// isLocalDevEnv reports whether MOLECULE_ENV names a local-dev environment
// ("development" / "dev"). It carries NO authentication semantics — callers
// must never use it to bypass a credential check. It exists only for
// dev-convenience / defense-in-depth knobs (rate-limit relaxation, loopback
// bind default) that cannot expose protected data.
func isLocalDevEnv() bool {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("MOLECULE_ENV")))
	_, ok := devModeEnvValues[env]
	return ok
}

// IsLocalDevEnv exposes isLocalDevEnv to packages outside the middleware
// module (cmd/server bind-host default). NON-security: see isLocalDevEnv.
func IsLocalDevEnv() bool {
	return isLocalDevEnv()
}
