# Runbook — Handlers Postgres Integration port-collision substrate

**Status:** Resolved 2026-05-08 (PR for class B Hongming-owned CICD red sweep).

## Symptom

`Handlers Postgres Integration` workflow fails on staging push and PRs.
Step `Apply migrations to Postgres service` shows:

```
psql: error: connection to server at "127.0.0.1", port 5432 failed: Connection refused
```

Job-cleanup step further down logs:

```
Cleaning up services for job Handlers Postgres Integration
failed to remove container: Error response from daemon: No such container: <id>
```

…confirming the postgres service container was already gone before
cleanup ran.

## Root cause

Our Gitea act_runner (operator host `5.78.80.188`,
`/opt/molecule/runners/config.yaml`) sets:

```yaml
container:
  network: host
```

…which act_runner applies to BOTH the job container AND every
`services:` container in a workflow. Multiple workflow instances
running concurrently across the 16 parallel runners each try to bind
postgres on `0.0.0.0:5432`. The first wins; subsequent instances exit
immediately with:

```
LOG:  could not bind IPv4 address "0.0.0.0": Address in use
HINT: Is another postmaster already running on port 5432?
FATAL: could not create any TCP/IP sockets
```

act_runner sets `AutoRemove:true` on service containers, so Docker
garbage-collects them as soon as they exit. By the time the migrations
step runs `pg_isready` / `psql`, the container is gone and connection
refused.

Reproduction (operator host):

```bash
docker run --rm -d --name pg-A --network host \
  -e POSTGRES_PASSWORD=test postgres:15-alpine
docker run -d --name pg-B --network host \
  -e POSTGRES_PASSWORD=test postgres:15-alpine
docker logs pg-B   # FATAL: could not create any TCP/IP sockets
```

## Why per-job override doesn't work

The natural fix — per-job `container.network` override — is silently
ignored by act_runner. The runner log emits:

```
--network and --net in the options will be ignored.
```

This is a documented act_runner constraint: container network is a
runner-wide setting, not per-job. Source: gitea/act_runner config docs
+ vegardit/docker-gitea-act-runner issue #7.

Flipping the global `container.network` to `bridge` would break every
other workflow in the repo (cache server discovery,
`molecule-core-net` peer access during integration tests, etc.) —
unacceptable blast radius for a per-test bug.

## Fix shape

`handlers-postgres-integration.yml` no longer uses `services: postgres:`.
It launches a sibling postgres container manually on the existing
`molecule-core-net` bridge network with a per-run unique name:

```yaml
env:
  PG_NAME: pg-handlers-${{ github.run_id }}-${{ github.run_attempt }}
  PG_NETWORK: molecule-core-net

steps:
  - name: Start sibling Postgres on bridge network
    run: |
      docker run -d --name "${PG_NAME}" --network "${PG_NETWORK}" \
        ...
        postgres:15-alpine
      PG_HOST=$(docker inspect "${PG_NAME}" \
        --format "{{(index .NetworkSettings.Networks \"${PG_NETWORK}\").IPAddress}}")
      echo "PG_HOST=${PG_HOST}" >> "$GITHUB_ENV"

  # … migrations + tests use ${PG_HOST}, not 127.0.0.1 …

  - if: always() && …
    name: Stop sibling Postgres
    run: docker rm -f "${PG_NAME}" || true
```

The host-net job container can reach a bridge-net container via the
bridge IP directly (verified manually, 2026-05-08). Two parallel runs
use different names + different bridge IPs — no collision.

## Future-proofing

Other workflows that hit the same shape (any `services:` with a
fixed-port image) will exhibit the same failure mode under
host-network runner config. Translate using this same pattern:

1. Drop the `services:` block.
2. Use `${{ github.run_id }}-${{ github.run_attempt }}` for unique
   container name.
3. Launch on `molecule-core-net` (already trusted bridge in
   `docker-compose.infra.yml`).
4. Read back the bridge IP via `docker inspect` and export as a step env.
5. `if: always()` cleanup step at the end.

If the count of such workflows grows, factor into a composite action
(`./.github/actions/sibling-postgres`) so the substrate logic lives
in one place.

## Related

- Issue #88 (closed by #92): localhost → 127.0.0.1 fix that unmasked
  this collision; the IPv6 fix is correct, port collision is the new
  layer.
- Issue #94 created `molecule-core-net` + `alpine:latest` as
  prereqs.
- Saved memory `feedback_act_runner_github_server_url` documents
  another act_runner-vs-GHA divergence (server URL).
