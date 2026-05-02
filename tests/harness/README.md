# Production-shape local harness

The harness brings up the SaaS tenant topology on localhost using the
same `Dockerfile.tenant` image that ships to production. Tests target
the cf-proxy on `http://localhost:8080` and pass the tenant identity
via a `Host:` header — exactly the way production CF tunnel routes by
Host header. The cf-proxy nginx then rewrites headers and proxies to
the right tenant container, exercising the SAME code path a real tenant
takes including TenantGuard middleware, the `/cp/*` reverse proxy, the
canvas reverse proxy, and a Cloudflare-tunnel-shape header rewrite
layer.

Since Phase 2 the harness runs **two tenants in parallel** (alpha and
beta) with their own Postgres instance and distinct
`MOLECULE_ORG_ID`s — same shape as production, where each tenant gets
its own EC2 + DB. This is what cross-tenant isolation replays need to
prove TenantGuard actually 404s a misrouted request.

`tests/harness/_curl.sh` is the helper sourced by every replay. Per
tenant: `curl_alpha_anon` / `curl_alpha_admin` / `curl_beta_anon` /
`curl_beta_admin` / `psql_exec_alpha` / `psql_exec_beta`. Plus
deliberately-wrong cross-tenant negative-test helpers for isolation
replays: `curl_alpha_creds_at_beta` / `curl_beta_creds_at_alpha`.
Legacy single-tenant aliases (`curl_anon`, `curl_admin`, `psql_exec`)
default to alpha so pre-Phase-2 replays continue to work. New replays
should source `_curl.sh` rather than rolling their own curl.

## Why this exists

Local `go run ./cmd/server` skips:
- `TenantGuard` middleware (no `MOLECULE_ORG_ID` env)
- `/cp/*` reverse proxy mount (no `CP_UPSTREAM_URL` env)
- `CANVAS_PROXY_URL` (canvas runs separately on `:3000`)
- Header rewrites that production's CF tunnel + LB perform
- Strict-auth mode (no live `ADMIN_TOKEN`)

Bugs that survive `go run` and ship to production almost always live
in one of those layers. The harness activates ALL of them.

## Topology

```
                                      client
                                        ↓
                                     cf-proxy            nginx, mirrors CF tunnel header rewrites
                                        ↓ (routes by Host header)
              ┌─────────────────────────┴─────────────────────────┐
              ↓                                                   ↓
        tenant-alpha                                        tenant-beta
        Host: harness-tenant-alpha.localhost                Host: harness-tenant-beta.localhost
        MOLECULE_ORG_ID=harness-org-alpha                   MOLECULE_ORG_ID=harness-org-beta
              ↓                                                   ↓
        postgres-alpha                                      postgres-beta
              ↓                                                   ↓
              └─────────────────────────┬─────────────────────────┘
                                        ↓
                             cp-stub + redis (shared)
```

Each tenant runs the production `Dockerfile.tenant` image with its own
admin token, org id, and Postgres instance — identical isolation
boundaries to production where each tenant gets a dedicated EC2 + DB.
cp-stub and redis are shared because they model the per-region
multi-tenant CP and a single Redis cluster.

## Quickstart

```bash
cd tests/harness
./up.sh                 # builds + starts all services (both tenants)
./seed.sh               # registers parent+child workspaces in BOTH tenants
./replays/tenant-isolation.sh
./replays/per-tenant-independence.sh
./down.sh               # tear down + remove volumes
```

To run every replay in one shot (boot, seed, run-all, teardown):

```bash
cd tests/harness
./run-all-replays.sh    # full lifecycle; non-zero exit if any replay fails
KEEP_UP=1 ./run-all-replays.sh   # leave harness up for debugging
REBUILD=1 ./run-all-replays.sh   # rebuild images before booting
```

No `/etc/hosts` edit required — replays use the cf-proxy's loopback
port and pass the per-tenant `Host:` header (`_curl.sh` handles this
automatically). This matches how production CF tunnel routes: the URL
is the public CF endpoint, the Host header carries the per-tenant
identity. Quick check:

```bash
curl -H "Host: harness-tenant-alpha.localhost" http://localhost:8080/health
curl -H "Host: harness-tenant-beta.localhost"  http://localhost:8080/health
```

(If you have a legacy `/etc/hosts` entry from older docs, it still
works — `BASE`, `ALPHA_HOST`, `BETA_HOST` all honor env-var overrides.
The legacy `harness-tenant.localhost` host alias maps to alpha.)

## Replay scripts

Each replay script reproduces a real bug class against the harness so
fixes can be verified locally before deploy. The bar for adding a
replay is "this bug shipped to production despite local E2E being
green" — the script becomes the regression gate that closes that gap.

| Replay | Closes | What it proves |
|--------|--------|----------------|
| `peer-discovery-404.sh` | #2397 | tool_list_peers surfaces the actual reason instead of "may be isolated" |
| `buildinfo-stale-image.sh` | #2395 | GIT_SHA reaches the binary; verify-step comparison logic works |
| `chat-history.sh` | #2472 + #2474 + #2476 | `peer_id` filter (incl. OR over source/target) + `before_ts` paging + UUID/RFC3339 trust boundary on the activity route |
| `channel-envelope-trust-boundary.sh` | #2471 + #2481 | published wheel scrubs malformed `peer_id` from the channel envelope and from `agent_card_url` (path-traversal + XML-attr injection) |
| `tenant-isolation.sh` | Phase 2 | TenantGuard 404s any request whose `X-Molecule-Org-Id` doesn't match the container's `MOLECULE_ORG_ID` (covers cross-tenant routing bug + allowlist drift); per-tenant `/workspaces` listings stay partitioned |
| `per-tenant-independence.sh` | Phase 2 | parallel A2A workflows in both tenants don't bleed into each other's `activity_logs` / `workspaces`, including under a concurrent INSERT race (catches lib/pq prepared-statement cache collision + shared-pool poisoning) |

To add a new replay:
1. Drop a script under `replays/` named after the issue.
2. The script's purpose: reproduce the production failure mode against
   the harness, then assert the fix is present. PASS criterion is the
   post-fix behavior.
3. The `run-all-replays.sh` runner picks up every `replays/*.sh` script
   automatically — no per-replay registration needed.

## Extending the cp-stub

`cp-stub/main.go` serves the minimum surface for the existing replays
plus a catch-all that returns 501 + a clear message when the tenant
asks for a route the stub doesn't implement. To add a new CP route:

1. Add a `mux.HandleFunc` in `cp-stub/main.go` for the path.
2. Return the same wire shape the real CP returns. The contract is
   "wire compatibility with the staging CP at the time of writing" —
   document it with a comment pointing at the real CP handler.
3. Add a replay script that exercises the path.

## What the harness does NOT cover

- Real TLS / cert handling (CF terminates TLS in production; harness is
  HTTP-only).
- Cloudflare API edge cases (rate limits, DNS propagation timing).
- Real EC2 / SSM / EBS behavior (image-cache replay simulates the
  outcome but not the AWS API surface).
- Cross-region or multi-AZ topology.
- Real production data scale.

These are intentional Phase 1 limits. If a bug class hits one of these
gaps, escalate to staging E2E rather than expanding the harness past
its mandate of "exercise the tenant binary in production-shape topology."

## Roadmap

- **Phase 1 (shipped):** harness + cp-stub + cf-proxy + 4 replays + `run-all-replays.sh` runner. No-sudo `Host`-header path via `_curl.sh`. Per-replay psql seeding for tests that need DB-side fixtures.
- **Phase 2 (shipped):** multi-tenant — `tenant-alpha` + `tenant-beta` with their own Postgres instances and distinct `MOLECULE_ORG_ID`s; cf-proxy nginx routes by Host header (prod CF tunnel parity); `seed.sh` registers parent+child workspaces in both tenants; `_curl.sh` exposes per-tenant + cross-tenant-negative helpers; new replays cover TenantGuard isolation (`tenant-isolation.sh`) and per-tenant independence under concurrent load (`per-tenant-independence.sh`). `harness-replays.yml` runs `run-all-replays.sh` as a required check on every PR touching `workspace-server/**`, `canvas/**`, `tests/harness/**`, or the workflow itself.
- **Phase 3:** replace `cp-stub/` with the real `molecule-controlplane` Docker build. Add a config-coherence lint that diffs harness env list against production CP's env list and fails CI on drift. Convert `tests/e2e/test_api.sh` to target the harness instead of localhost.
- **Phase 4 (long-term):** Miniflare in front of cf-proxy for real CF emulation (WAF, BotID, rate-limit, cf-tunnel headers). LocalStack for the EC2 provisioner. Anonymized prod-traffic recording/replay for SaaS-scale regression detection.
