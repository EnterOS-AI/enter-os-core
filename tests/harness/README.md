# Production-shape local harness

The harness brings up the SaaS tenant topology on localhost using the
same `Dockerfile.tenant` image that ships to production. Tests run
against `http://harness-tenant.localhost:8080` and exercise the
SAME code path a real tenant takes — including TenantGuard middleware,
the `/cp/*` reverse proxy, the canvas reverse proxy, and a
Cloudflare-tunnel-shape header rewrite layer.

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
cf-proxy        nginx, mirrors CF tunnel header rewrites
  ↓ (Host:harness-tenant.localhost, X-Forwarded-*)
tenant          workspace-server/Dockerfile.tenant — same image as prod
  ↓ (CP_UPSTREAM_URL=http://cp-stub:9090, /cp/* proxied)
cp-stub         minimal Go service, mocks CP wire surface
postgres        same version as production
redis           same version as production
```

## Quickstart

```bash
cd tests/harness
./up.sh                 # builds + starts all services
./seed.sh               # mints admin token, registers two sample workspaces
./replays/peer-discovery-404.sh
./replays/buildinfo-stale-image.sh
./down.sh               # tear down + remove volumes
```

First-time setup needs an `/etc/hosts` entry so `harness-tenant.localhost`
resolves to the local cf-proxy:

```bash
echo "127.0.0.1 harness-tenant.localhost" | sudo tee -a /etc/hosts
```

(macOS resolves `*.localhost` automatically in some setups; Linux
typically does not.)

## Replay scripts

Each replay script reproduces a real bug class against the harness so
fixes can be verified locally before deploy. The bar for adding a
replay is "this bug shipped to production despite local E2E being
green" — the script becomes the regression gate that closes that gap.

| Replay | Closes | What it proves |
|--------|--------|----------------|
| `peer-discovery-404.sh` | #2397 | tool_list_peers surfaces the actual reason instead of "may be isolated" |
| `buildinfo-stale-image.sh` | #2395 | GIT_SHA reaches the binary; verify-step comparison logic works |

To add a new replay:
1. Drop a script under `replays/` named after the issue.
2. The script's purpose: reproduce the production failure mode against
   the harness, then assert the fix is present. PASS criterion is the
   post-fix behavior.
3. Wire it into the `tests/harness/run-all-replays.sh` runner (TODO,
   Phase 2).

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

- **Phase 1 (this PR):** harness + cp-stub + cf-proxy + 2 replays.
- **Phase 2:** convert `tests/e2e/test_api.sh` to run against the
  harness instead of localhost. Make harness-based E2E a required CI
  check.
- **Phase 3:** config-coherence lint that diffs harness env list
  against production CP's env list, fails CI on drift.
