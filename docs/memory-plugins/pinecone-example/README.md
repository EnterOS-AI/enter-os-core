# Pinecone-backed Memory Plugin (worked example)

A working sketch of a memory plugin that delegates storage to
[Pinecone](https://www.pinecone.io/) instead of postgres.

This is **example code, not a production binary**. It demonstrates
how to map the v1 contract onto a vector database. Operators who
want to ship this would harden auth, add retries, batch the
commit path, etc.

## Why Pinecone is interesting

The default postgres plugin's pgvector index works for ~10M memories
on a single node. Beyond that, semantic search becomes painful. A
managed vector database can handle 1B+ memories, but the trade-offs
are different:

- **Capabilities**: Pinecone is great at `embedding` (its core
  feature) but has no first-class FTS. So the plugin reports
  `["embedding"]` and ignores the `query` field.
- **TTL**: Pinecone supports per-vector metadata with deletion via
  metadata filter — TTL becomes a periodic janitor task, not a
  per-row property.
- **Cost**: per-vector billing, so the plugin should batch writes
  and dedup before posting.

## Wire mapping

| Contract field | Pinecone shape |
|---|---|
| `namespace` | `namespace` (Pinecone's first-class concept) |
| `id` (caller-supplied) | `id` (Pinecone vector id; plugin upserts on this) |
| `id` (omitted) | Plugin generates `uuid.NewString()` before upsert |
| `content` | metadata.text |
| `embedding` | `values` |
| `kind` / `source` / `pin` / `expires_at` | `metadata.{kind, source, pin, expires_at}` |
| `propagation` (opaque JSON) | `metadata.propagation` (also opaque) |

The contract's `expires_at` becomes a metadata field; a separate
janitor cron periodically queries `expires_at < now` and deletes.

Pinecone's native upsert is the right fit for the idempotency-key
contract: passing the same `id` twice updates in place. So a
Pinecone plugin gets idempotent backfill retries "for free" if it
just forwards `MemoryWrite.id` (or its generated UUID) to the
upsert call.

## Skeleton

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "net/http"
    "os"

    "github.com/pinecone-io/go-pinecone/pinecone"
)

type pineconePlugin struct {
    client *pinecone.Client
    index  string
}

func main() {
    apiKey := os.Getenv("PINECONE_API_KEY")
    if apiKey == "" {
        log.Fatal("PINECONE_API_KEY required")
    }
    client, err := pinecone.NewClient(pinecone.NewClientParams{ApiKey: apiKey})
    if err != nil {
        log.Fatal(err)
    }
    p := &pineconePlugin{client: client, index: os.Getenv("PINECONE_INDEX")}

    http.HandleFunc("/v1/health", p.health)
    http.HandleFunc("/v1/search", p.search)
    // ... rest of the routes ...

    log.Fatal(http.ListenAndServe(":9100", nil))
}

func (p *pineconePlugin) health(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "status":       "ok",
        "version":      "1.0.0",
        "capabilities": []string{"embedding"}, // no FTS, no TTL out-of-box
    })
}

func (p *pineconePlugin) search(w http.ResponseWriter, r *http.Request) {
    // Parse contract.SearchRequest
    // Build Pinecone QueryByVectorValuesRequest with body.Embedding
    // For each Pinecone namespace in body.Namespaces, call Query
    // Map results to contract.Memory
    // ...
}
```

## What's missing from this sketch

A production-ready Pinecone plugin would add:

- **Batch commits**: bulk upsert N memories in a single Pinecone call
- **TTL janitor**: periodic deletion of expired vectors
- **Connection pooling**: keep one Pinecone client alive across requests
- **Retry + circuit breaker**: Pinecone occasionally returns 5xx
- **Metrics**: latency histograms per endpoint, write/read counters
- **Idempotency-key handling**: when `MemoryWrite.id` is supplied,
  forward it as the Pinecone vector id verbatim; otherwise generate
  one. Pinecone's `Upsert` is naturally idempotent on id match.

But the mapping above is the load-bearing part — the rest is
operational hardening, not contract-specific.

## See also

- [Pinecone Go SDK docs](https://docs.pinecone.io/reference/go-sdk)
- [Memory plugin contract spec](../../api-protocol/memory-plugin-v1.yaml)
- [Default postgres plugin source](../../../workspace-server/cmd/memory-plugin-postgres/) — for comparison
