# Real-subprocess E2E for memory-plugin-postgres

The default `go test ./...` suite covers the plugin via in-process
sqlmock tests (PR-3). This directory ALSO ships build-tag-gated tests
that spawn the real binary against a live postgres — to catch
classes of bug in-process tests can't see:

- Boot-path regressions (env var typos, panic-on-startup)
- Wire-format bugs sqlmock smooths over (the `pq.Array` issue we
  hit during PR-3 development)
- HTTP/socket encoding edge cases
- C1 idempotency (real upsert against real postgres)

## Running

The tests skip silently unless an operator opts in with both:
- The `memory_plugin_e2e` build tag
- `MEMORY_PLUGIN_E2E_DB` env var pointing at a writable postgres

### Quick local run (with docker)

```bash
docker run --rm -d --name memory-plugin-e2e-pg \
  -e POSTGRES_PASSWORD=test -e POSTGRES_USER=test -e POSTGRES_DB=test \
  -p 5432:5432 \
  pgvector/pgvector:pg16

# Wait a few seconds for postgres to accept connections
until docker exec memory-plugin-e2e-pg pg_isready -U test >/dev/null 2>&1; do sleep 0.5; done

MEMORY_PLUGIN_E2E_DB=postgres://test:test@localhost:5432/test?sslmode=disable \
  go test -tags memory_plugin_e2e -v -count=1 ./cmd/memory-plugin-postgres/

docker stop memory-plugin-e2e-pg
```

### CI integration

These tests are NOT in the default required-checks set. Operators
gating cutover on the suite should add a separate workflow step:

```yaml
- name: Memory plugin E2E
  if: ${{ contains(github.event.pull_request.labels.*.name, 'memory-v2') }}
  run: |
    MEMORY_PLUGIN_E2E_DB=${{ secrets.MEMORY_PLUGIN_TEST_DSN }} \
      go test -tags memory_plugin_e2e -v -count=1 ./cmd/memory-plugin-postgres/
```

## What each test pins

| Test | Covers |
|---|---|
| `TestE2E_BootAndHealth` | Binary builds, starts, advertises all 5 capabilities |
| `TestE2E_FullCommitSearchForgetRoundTrip` | Real wire encoding (no sqlmock), full agent flow |
| `TestE2E_IdempotencyKey` | C1 fix end-to-end — upserts against real postgres |

## What's still NOT covered

- Migration drift (assumes the migrations dir is at the conventional
  path; operator-customized layouts need their own test)
- Plugin-internal recovery (kill backing store mid-request, etc.)
- Concurrent commits with id collisions across processes
- TTL eviction (would need to extend test runtime past `expires_at`)

These gaps apply equally to forks of this binary; they're listed in
[`testing-your-plugin.md`](../../../docs/memory-plugins/testing-your-plugin.md)
under "what the harness does NOT cover".
