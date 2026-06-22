# E2E Testing

End-to-end test scripts live under `tests/e2e/` and exercise the platform against a real Postgres + Redis. Every script is shellcheck-clean and shares helpers from `tests/e2e/_lib.sh` + `tests/e2e/_extract_token.py`.

## Scripts

| Script | Checks | Prerequisites |
|--------|--------|--------------|
| `test_api.sh` | 62 | platform running on :8080; no live agents required |
| `test_comprehensive_e2e.sh` | 67 | platform running; spins up its own workspaces |
| `test_a2a_e2e.sh` | 22 | platform + 2 provisioned agents (Echo + SEO) with `OPENROUTER_API_KEY` |
| `test_activity_e2e.sh` | 25 | platform + 1 online agent |
| `test_claude_code_e2e.sh` | — | platform + Claude Code runtime; exercises CLI adapter |

## Auth Prerequisites (Phase 30)

After Phase 30.1, the following routes require `Authorization: Bearer <token>` once a workspace has any live token on file (legacy workspaces are grandfathered):

- `POST /registry/heartbeat`
- `POST /registry/update-card`

After Phase 30.6, the following routes additionally require `X-Workspace-ID` on the caller side (bearer token validated, fail-open on DB hiccup):

- `GET /registry/discover/:id`
- `GET /registry/:id/peers`

The scripts handle this by:

1. Creating a workspace → platform returns no token yet.
2. Calling `POST /registry/register` — response body includes `auth_token` once per workspace.
3. Extracting the token via `_extract_token.py` (reads JSON from stdin).
4. Passing it in subsequent heartbeat / discover / peers calls.

`test_comprehensive_e2e.sh` registers each workspace **immediately after creation** so the provisioner's auto-register doesn't race the test's explicit register. `test_activity_e2e.sh` re-registers a detected-already-online agent to capture a fresh bearer token.

## Running Locally

```bash
# Quickest check after any platform change:
cd workspace-server && go build ./cmd/server && ./server &
bash tests/e2e/test_api.sh        # expect 62/62 pass

# Comprehensive sweep:
bash tests/e2e/test_comprehensive_e2e.sh   # expect 67/67 pass
```

Both scripts include a pre-test cleanup that deletes workspaces from previous runs so a stale DB won't cause spurious failures.

## What CI Runs

`.github/workflows/ci.yml` (added 2026-04-13):

- **e2e-api** — spins up Postgres + Redis via service containers, applies migrations with `docker exec`, builds the platform binary, runs `tests/e2e/test_api.sh`. All 62 checks must pass.
- **shellcheck** — runs the shellcheck marketplace action against every `tests/e2e/*.sh`.

The other E2E scripts are not yet in CI because they require provisioned agents and LLM credentials; run them locally before merging runtime-touching changes.

## Adding a New E2E Check

1. Source `tests/e2e/_lib.sh` for `assert_*` helpers, bearer-token extraction, and the cleanup preamble.
2. When hitting an auth-gated route, always register the workspace first and thread the returned token through subsequent requests.
3. Keep each check idempotent — the comprehensive script is expected to be re-runnable on the same DB.
4. Run `shellcheck tests/e2e/your_script.sh` locally before pushing.

## Related Docs

- [Local Development](./local-development.md)
- [Platform API](../api-protocol/platform-api.md) — route reference incl. auth requirements
