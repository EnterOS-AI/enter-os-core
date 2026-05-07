# Rate-limit observability runbook

> Companion to issue #64 ("RATE_LIMIT default re-tune analysis"). After
> #60 deployed the per-tenant `keyFor` keying, the right RATE_LIMIT
> default became data-dependent. This runbook documents the metrics +
> queries an operator should run to confirm whether the current 600
> req/min/key default is correct, too tight, or too loose.

## What's already exposed

The workspace-server's existing Prometheus middleware
(`workspace-server/internal/metrics/metrics.go`) tracks every request
on every path:

```
molecule_http_requests_total{method, path, status}      counter
molecule_http_request_duration_seconds_total{method,path,status}  counter
```

Path is the matched route pattern (`/workspaces/:id/activity` etc), so
high-cardinality workspace UUIDs do not explode the label space.

The rate limiter middleware (#60, `workspace-server/internal/middleware/ratelimit.go`)
also stamps every response with `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
and `X-RateLimit-Reset`. Operators with browser-side or proxy-side
header capture can read per-request bucket state directly.

No new instrumentation is needed for #64's acceptance criteria. The
metric surface is sufficient — this runbook just collects the queries.

## Queries to run after #60 deploys

### 1. Is the bucket actually firing 429s?

```promql
sum(rate(molecule_http_requests_total{status="429"}[5m]))
```

If this is zero on a given tenant, the bucket isn't being hit. If it's
sustained > 1/min, dig in.

### 2. Which routes attract 429s?

```promql
topk(
  10,
  sum by (path) (
    rate(molecule_http_requests_total{status="429"}[5m])
  )
)
```

Expected shape post-#60:
- `/workspaces/:id/activity` should be near zero — the canvas no longer
  polls it on a 30s/60s/5s cadence (PRs #69 / #71 / #76).
- Probe / health / heartbeat paths should be ~0 (those routes have a
  separate IP-fallback bucket).

If `/workspaces/:id/activity` 429s persist post-PRs-69/71/76 deploy, the
canvas isn't running the WS-subscriber path — investigate WS health
on that tenant.

### 3. Per-bucket-key inference (no direct exposure today)

The bucket map itself is in-memory only; we deliberately do **not**
expose `org:<uuid>` ↔ remaining-tokens because that map can include
SHA-256 hashes of bearer tokens. A tenant that wants per-key visibility
should rely on response headers (`X-RateLimit-Remaining` on every
response from a given session is the bucket's view of that session).

If you genuinely need server-side per-bucket counts for triage,
file a follow-up — the proper shape is a `/internal/ratelimit-stats`
endpoint that emits **counts per key prefix only** (e.g. `org:`, `tok:`,
`ip:`), never the key payloads. Don't roll that ad-hoc; it's a security
review surface.

## Decision tree for the re-tune

After 14 days of production traffic on a tenant, look at the queries
above and walk this tree:

```
Q1: Is the 429 rate sustained > 0.1/sec on any tenant?
  ├─ NO  → The 600 default has comfortable headroom. Either keep it,
  │        or lower it carefully (300) ONLY if you have a documented
  │        reason (e.g. a misbehaving client we want to throttle harder).
  │        Default to "no change" — see #64 for the math.
  └─ YES → Q2.

Q2: Is the 429 rate concentrated on ONE tenant or spread across many?
  ├─ ONE tenant → Operator override: set RATE_LIMIT=1200 or 1800 on that
  │               tenant's box. Document in the tenant's ops note. The
  │               default does not need to change.
  └─ MANY tenants → Q3.

Q3: Are the 429s on a route that polls (e.g. /activity / /peers)?
  ├─ YES → Confirm PRs #69, #71, #76 have actually deployed to those
  │         tenants. If they have and 429s persist, the canvas may have
  │         a regression — do not raise RATE_LIMIT. File a canvas issue.
  └─ NO  → 429s on mutating routes mean genuine load. Raise the default
            to 1200 in `workspace-server/internal/router/router.go:54`.
            Same PR should attach: the metric chart, the time window,
            and a paragraph explaining what changed in our traffic shape.
```

## Alert rule template (drop-in for Prometheus)

```yaml
# Sustained 429s — file is the SLO trip-wire. If this fires, walk the
# decision tree above. NB: the issue#64 acceptance criterion is "two
# weeks of metrics"; this alert is the inverse — it tells you something
# changed before the two weeks are up.
groups:
  - name: workspace-server-ratelimit
    rules:
      - alert: WorkspaceServerRateLimit429Sustained
        expr: |
          sum by (instance) (
            rate(molecule_http_requests_total{status="429"}[10m])
          ) > 0.1
        for: 30m
        labels:
          severity: warning
          owner: workspace-server
        annotations:
          summary: "{{ $labels.instance }} sustained 429s — see ratelimit-observability runbook"
          runbook: "https://git.moleculesai.app/molecule-ai/molecule-core/blob/main/docs/engineering/ratelimit-observability.md"
```

Threshold rationale: 0.1 req/s = 6/min sustained over 10min. Below
that, a 429 is almost certainly a transient burst that the canvas's
retry-once handler at `canvas/src/lib/api.ts:55` already absorbs. The
30m `for:` keeps the alert from chattering on a brief blip.

## Companion probe script

For one-off triage when an operator can reproduce the problem in their
own browser, `scripts/edge-429-probe.sh` (#62) reproduces a canvas-
sized burst against a tenant subdomain and dumps each 429's response
shape so the operator can distinguish workspace-server bucket overflow
from CF/Vercel edge rate-limiting without dashboard access.

```sh
./scripts/edge-429-probe.sh hongming.moleculesai.app --burst 80 --out /tmp/edge.txt
```

The script's report header explains how to read the output.
