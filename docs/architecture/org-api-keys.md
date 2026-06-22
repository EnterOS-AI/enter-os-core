# Organization API Keys

> **Status:** Shipped (beta), 2026-04-20. See `docs/guides/org-api-keys.md` for user-facing usage.

Full-admin bearer tokens scoped to a single tenant org. User-visible
replacement for the single `ADMIN_TOKEN` env var — named, revocable,
audited, mintable from the canvas UI without ops intervention.

## Why this exists

Before these, admin access on a tenant required the bootstrap
`ADMIN_TOKEN` from AWS Secrets Manager. That token:

- Is a single shared value with no name or audit trail
- Can't be rotated without redeploying the tenant
- Is inaccessible to users (stored in ops-only SM)
- Can't be revoked individually — rotating it kills every integration

For the beta growth phase we want users to hand an AI agent an API
key and not worry about ops. Org API keys solve that: mint, use,
revoke, all from the canvas UI.

## Data model

```sql
CREATE TABLE org_api_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash    BYTEA NOT NULL,     -- sha256(plaintext)
    prefix        TEXT  NOT NULL,     -- first 8 plaintext chars for UI
    name          TEXT,               -- user label ("zapier", "ci-bot")
    created_by    TEXT,               -- provenance: "session"/"org-token:xxxxxxxx"/"admin-token"
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    UNIQUE (token_hash)
);

CREATE INDEX org_api_tokens_live_idx
    ON org_api_tokens (token_hash)
    WHERE revoked_at IS NULL;
```

Plaintext is NEVER stored. Only sha256 hash. Recovery is impossible
— lost tokens must be revoked and replaced.

The partial index keeps the hot-path `SELECT id WHERE token_hash=$1
AND revoked_at IS NULL` O(log live-tokens) regardless of how many
tokens have been minted + revoked over the tenant's lifetime.

## Request flow

```
Browser / CLI / Agent
   │  Authorization: Bearer <plaintext>
   ▼
Cloudflare edge
   │
   ▼  tunnel (path-matched)
Tenant platform :8080
   │
   ▼  TenantGuard (allowed; same-origin or header)
   ▼  AdminAuth middleware
       ├ Tier 0: fail-open (only if no ADMIN_TOKEN and no live tokens)
       ├ Tier 1: CP session cookie → /cp/auth/tenant-member
       ├ Tier 2a: sha256(bearer) IN org_api_tokens WHERE revoked_at IS NULL   ← THIS
       ├ Tier 2b: bearer == ADMIN_TOKEN (bootstrap / break-glass)
       └ Tier 3: any live workspace token (deprecated, only if no ADMIN_TOKEN)
```

Cost per request on the hot path: ONE indexed SELECT + one async
last_used_at UPDATE. Both hit the partial index; negligible vs
everything else the request does.

## Authorization scope

Every live org API token grants the SAME access as `ADMIN_TOKEN`:

- All `/workspaces/*` CRUD (create, delete, list, any workspace's sub-routes)
- All `/approvals/pending`, `/bundles/import`, `/org/import`, `/org/templates`
- All `/admin/*` routes
- All `/settings/secrets`, `/channels/discover`, `/events/*`
- Mint + revoke other org API tokens (self-sustaining after bootstrap)

It does NOT grant:

- Access to the control plane (`/cp/*`) directly — those are proxied
  by the tenant and the CP has its own auth (WorkOS session). An
  org token alone can't hit `/cp/admin/orgs` or `/cp/billing/*`.
- Cross-tenant access — each tenant's `org_api_tokens` table is
  isolated in its own Postgres.

## Bootstrap + self-sustenance

The FIRST org token on a fresh tenant is minted via either:

1. **Canvas UI**: a user with a WorkOS session cookie (verified via
   `/cp/auth/tenant-member`) opens Settings → Org API Keys → New.
2. **ADMIN_TOKEN CLI**: `curl -XPOST /org/tokens -H "Authorization:
   Bearer $ADMIN_TOKEN"`. Useful in provisioning scripts or when
   the canvas is down.

After that, any existing org token can mint more. Revocation
leaves ADMIN_TOKEN as the break-glass credential — operators can
still recover admin access even if every user-minted token is
revoked.

## Security properties

- **Plaintext never persisted**: only sha256 hash. A DB leak gives
  the attacker prefixes + hashes — neither lets them forge a token.
- **Timing-safe lookup**: single hash-indexed SELECT. No
  path-dependent branches that could leak hash-prefix info.
- **Immediate revocation**: `UPDATE revoked_at = now()` takes
  microseconds; the next request returns 401. Partial index means
  no lag from rebuilding full indexes.
- **Idempotent revoke**: revoking twice returns 404 the second
  time, not a conflict. Simplifies revoke tooling that might
  double-deliver.
- **Collapsed failure responses**: `Validate()` returns
  `ErrInvalidToken` for any failure (bad bytes, revoked, deleted,
  never-existed). Response shape cannot distinguish, so enumeration
  is blind.
- **Audit trail via `created_by`**: every token row records its
  provenance ("session", "org-token:<prefix>", "admin-token") so
  post-incident review can follow a chain of mints.

## Threat model

| Threat | Mitigation |
|---|---|
| Attacker exfiltrates a token via leaked logs | Tokens NEVER logged at INFO — only prefixes. `created_by` audit shows who minted what. |
| Attacker cracks a stored hash | sha256 of 256 bits of uniform-random input — not crackable in our lifetime. Rainbow tables would need 2^256 entries. |
| Attacker brute-forces the bearer | 256 bits of entropy, base64url-encoded 43-char string. At 1e9 guesses/sec it would take >1e60 years. Rate limiting is not the primary defense here; entropy is. |
| Admin's session cookie is stolen | Cookie mints org tokens. Revoke the fresh tokens, rotate ADMIN_TOKEN, force WorkOS re-auth via logout. Mitigations: WorkOS session expiry + `created_by: session` audit trail makes post-hoc detection possible. |
| Token leaks to an AI that misbehaves | Full-org access — damage confined to the tenant but large within it. Beta trade-off accepted. **Future work:** scoped roles. |
| Tenant Postgres is compromised | Attacker can't forge tokens (only hashes stored). They CAN read workspace secrets — that's the separate secrets-encryption story (`SECRETS_ENCRYPTION_KEY`). |

## HTTP surface

```
GET    /org/tokens              list live tokens (prefix + metadata only)
POST   /org/tokens               mint; plaintext returned once
       body: {"name": "optional label"}
DELETE /org/tokens/:id           revoke; idempotent (404 on already-revoked)
```

All three behind `AdminAuth`. See `internal/handlers/org_tokens.go`.

## Known limitations

- Every token is full-org admin. Role scoping (admin / editor /
  reader) and per-workspace binding are planned but not shipped
  today.
- No expiry / TTL. Tokens live until explicitly revoked.
- Tokens minted via canvas session are audited as
  `created_by: "session"` without the WorkOS user_id. A specific
  user's mint activity can't be attributed from the table alone
  until the session identity is captured.
