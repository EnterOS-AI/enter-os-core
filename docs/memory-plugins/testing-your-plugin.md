# Testing Your Memory Plugin

Once you have a plugin implementing the v1 contract, you can validate
it against the spec without booting workspace-server.

## The contract test harness

Workspace-server ships typed Go bindings + round-trip tests in
`workspace-server/internal/memory/contract/`. The simplest way to
gain confidence in your plugin's wire compatibility is to point those
tests at it.

A minimal contract suite:

```go
package myplugin_test

import (
    "context"
    "testing"

    mclient "github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/client"
    "github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

func TestMyPlugin_FullRoundTrip(t *testing.T) {
    // Start your plugin somehow (subprocess, in-process, etc.)
    pluginURL := startMyPlugin(t)
    cl := mclient.New(mclient.Config{BaseURL: pluginURL})

    // 1. Health
    hr, err := cl.Boot(context.Background())
    if err != nil {
        t.Fatalf("Boot: %v", err)
    }
    if hr.Status != "ok" {
        t.Errorf("status = %q", hr.Status)
    }

    // 2. Namespace upsert
    if _, err := cl.UpsertNamespace(context.Background(), "workspace:test-1",
        contract.NamespaceUpsert{Kind: contract.NamespaceKindWorkspace}); err != nil {
        t.Fatalf("UpsertNamespace: %v", err)
    }

    // 3. Commit memory
    resp, err := cl.CommitMemory(context.Background(), "workspace:test-1",
        contract.MemoryWrite{
            Content: "hello",
            Kind:    contract.MemoryKindFact,
            Source:  contract.MemorySourceAgent,
        })
    if err != nil {
        t.Fatalf("CommitMemory: %v", err)
    }
    if resp.ID == "" {
        t.Errorf("plugin must return a non-empty memory id")
    }

    // 4. Search
    sresp, err := cl.Search(context.Background(), contract.SearchRequest{
        Namespaces: []string{"workspace:test-1"},
        Query:      "hello",
    })
    if err != nil {
        t.Fatalf("Search: %v", err)
    }
    if len(sresp.Memories) == 0 {
        t.Errorf("plugin returned no memories for the query we just wrote")
    }

    // 5. Forget
    if err := cl.ForgetMemory(context.Background(), resp.ID,
        contract.ForgetRequest{RequestedByNamespace: "workspace:test-1"}); err != nil {
        t.Errorf("ForgetMemory: %v", err)
    }
}
```

## What the harness does NOT cover

- **Capability accuracy**: if you list `embedding` you must actually
  do semantic search. The harness can't tell you whether ranking is
  meaningful — only that you don't crash.
- **TTL eviction**: write a memory with `expires_at` 1 second in the
  future, sleep 2 seconds, search — assert the memory is gone.
- **Concurrency**: hit your plugin with 100 parallel writes; assert
  no IDs collide.
- **Recovery**: kill your plugin's storage backend, send a request,
  assert your plugin returns 503 (not 200 with stale data).

## Smoke test against workspace-server

Once unit-level wire tests pass, run a real workspace-server with your
plugin URL:

```bash
DATABASE_URL=postgres://... \
MEMORY_PLUGIN_URL=http://localhost:9100 \
./workspace-server
```

Then ask an agent to call `commit_memory_v2` and `search_memory`. If
both round-trip cleanly, you're done.

For the full E2E flow (including the namespace resolver, MCP layer,
and security perimeter), see [PR-11's plugin-swap test](../../workspace-server/test/e2e/memory_plugin_swap_test.go).

## Reporting bugs

If you find a contract ambiguity or missing edge case, file an issue
against `Molecule-AI/molecule-core` referencing RFC #2728.
