# Plugin install sources

> **TL;DR** — plugin **sources** (where a plugin comes from) and plugin
> **shapes** (what's inside it) are independent axes. Both are pluggable.
> Today we ship two sources (`local`, `github`) and one shape adapter
> (`AgentskillsAdaptor`). Both layers scale the same way: write one new
> class, register it, done.

## The two axes

```
┌──────────────────────────────────────────────┐
│ SOURCE — where we fetch the plugin from      │
│                                              │
│   local://my-plugin                          │
│   github://owner/repo#v1.0                   │
│   clawhub://name@1.2                         │
│   https://example.com/plugin.tgz             │
│                                              │
│   registered via plugins.Registry            │
└──────────────────────────────────────────────┘
                     │
                     │  1. resolver.Fetch() → platform-local staging dir
                     │  2. tar + copy → workspace container /configs/plugins/<name>/
                     ▼
┌──────────────────────────────────────────────┐
│ SHAPE — what the plugin's files mean         │
│                                              │
│   agentskills.io format (SKILL.md + scripts) │
│   MCP server                                 │
│   DeepAgents sub-agent                       │
│   LangGraph sub-graph                        │
│                                              │
│   registered via plugins_registry resolver   │
│   inside the workspace runtime               │
└──────────────────────────────────────────────┘
```

Neither layer mandates the other. A plugin installed from `github://…`
might be agentskills-format. A plugin installed from `local://…` might
be an MCP server. A plugin installed from `clawhub://…` in the future
might be whatever shape ClawHub packs happen to be.

## Source API

`POST /workspaces/:id/plugins` takes a single `source` field:

```json
{"source": "local://my-plugin"}          // platform registry
{"source": "github://org/repo"}          // GitHub default branch (public repos only)
{"source": "github://org/repo#v1.2.0"}   // pinned tag/branch/sha

// Future: clawhub://, https://, oci:// — not registered by default.
// Call GET /plugins/sources to see what's actually wired.
```

`GET /plugins/sources` lists the currently registered schemes:

```bash
curl $PLATFORM/plugins/sources
{"schemes":["github","local"]}
```

## Registering a new source

```go
// workspace-server/internal/router/router.go
plgh := handlers.NewPluginsHandler(pluginsDir, dockerCli, wh.RestartByID).
    WithSourceResolver(NewClawhubResolver(clawhubToken))
```

A `SourceResolver` must satisfy:

```go
type SourceResolver interface {
    Scheme() string                                              // unique scheme name
    Fetch(ctx context.Context, spec, dst string) (string, error) // copy into dst, return plugin name
}
```

Implementations must honour `ctx` cancellation, clean up temp state on
error, and validate their spec format before hitting the network.

## Built-in sources

### `local` — filesystem

```
local://<name>
<name>              # bare name, same as above
```

Reads from the directory configured as `pluginsDir` at startup (defaults
to the repo's `plugins/` directory). Name must match
`^[a-z0-9][a-z0-9._-]*$`; path-traversal attempts are rejected pre-fetch.

### `github` — GitHub repository

```
github://<owner>/<repo>
github://<owner>/<repo>#<ref>
```

**Public repositories only.** The resolver performs an anonymous shallow
clone via the system `git` binary (the platform Dockerfile installs
`git`); it does not authenticate. Private-repo support is deliberately
out of scope for now — doing it safely requires per-tenant credential
storage, scope-limited tokens, and an audit trail, none of which have
been designed yet. Until that lands, private-repo installs fail at clone
time with a 404 (mapped from git's "repository not found" output).

Owner + repo names are length-bounded; refs cannot start with `-` to
prevent ref-as-flag injection. The resolver also passes `--` before the
URL when invoking git, as belt-and-braces defense.

### Future resolvers (not yet implemented)

- **`https://<tarball-url>`** — direct HTTP tarball install. Planned.
- **`clawhub://<name>@<version>`** — ClawHub registry install. Planned
  behind a third-party package dep on the ClawHub client.
- **`oci://<registry>/<image>:<tag>`** — OCI artifact install. Planned
  for enterprise registries.

Adding a resolver is a single Go file — see "Registering a new source"
above. The set of built-in resolvers is intentionally small; anything
beyond `local` + `github` is extension territory.

## Security model

**In scope (enforced):**

- Every resolver validates its spec format before any network or
  filesystem operation (regex + length caps).
- The handler re-validates the plugin name returned by the resolver
  before using it as a path component, so a hostile resolver can't
  smuggle a traversal name into `/configs/plugins/`.
- Request body is size-capped (`PLUGIN_INSTALL_BODY_MAX_BYTES`,
  default 64 KiB) to bound JSON-parser work.
- Fetch is timed out (`PLUGIN_INSTALL_FETCH_TIMEOUT`, default 5 min)
  so a slow/malicious source can't tie up a handler indefinitely.
- Staged tree is size-capped (`PLUGIN_INSTALL_MAX_DIR_BYTES`, default
  100 MiB) before copy into the container.
- Concurrent registry writes protected by RWMutex.

**Out of scope (not enforced):**

- **Plugin file contents are trusted.** Installing a plugin is a
  code-execution grant for the workspace's runtime. Audit plugin
  sources as you would any dependency.
- Network egress from resolvers isn't sandboxed (no netns/cgroup
  isolation around `git clone`). If you self-host EnterOS in a
  multi-tenant or untrusted-tenant context, restrict egress at the
  network layer — an egress firewall, VPC NAT with allowlist, or
  equivalent. The platform itself does not isolate resolver traffic.
- No signature or checksum verification on fetched content — planned
  alongside an OCI-based resolver where content addressability is
  native.
- No per-workspace rate limit on installs (platform-global rate limit
  applies via the standard middleware).

## Shapes — the other axis

See [agentskills-compat.md](agentskills-compat.md) for how the shape
layer works. The two are wired together but independent: the source
layer's job ends when plugin files are staged on disk; the shape layer
(per-runtime adapter inside the workspace) decides what to do with them
on workspace startup.
