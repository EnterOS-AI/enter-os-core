# Partner API Keys — Programmatic Org Management

> **Status:** Planned
>
> **Problem:** All CP endpoints require a WorkOS browser session (OAuth
> redirect → `mcp_session` cookie). This blocks: partner integrations,
> CI/CD automation, programmatic testing, marketplace resellers, and any
> non-browser client that needs to create/manage orgs.
>
> **Solution:** Add API key authentication as a parallel auth path alongside
> WorkOS sessions. Partners authenticate with `Authorization: Bearer mol_pk_*`
> and access the same org management endpoints.

---

## Architecture

```
Browser user:
  WorkOS AuthKit → OAuth redirect → mcp_session cookie → RequireSession()

Partner/API client:
  Authorization: Bearer mol_pk_xxxxx → ValidatePartnerKey() → same handlers

Both paths converge at the handler layer — the handler doesn't know or care
which auth method was used. It receives a validated identity context.
```

## Auth flow

```
POST /cp/orgs
  Authorization: Bearer mol_pk_live_a1b2c3d4e5f6...
  Content-Type: application/json

  {"slug": "acme", "name": "Acme Corp", "plan": "starter"}

→ CP middleware:
  1. Check Authorization header for "Bearer mol_pk_*" prefix
  2. SHA-256 hash the token
  3. Look up hash in partner_api_keys table
  4. Verify: not revoked, not expired, scopes include required scope
  5. Set auth context: { partner_id, partner_name, scopes, org_id }
  6. Continue to handler

→ Handler creates org as normal (same code path as browser flow)
→ 201 Created { id, slug, name, status: "provisioning" }
```

## Key format

```
mol_pk_live_<32 random hex chars>    — production key
mol_pk_test_<32 random hex chars>    — test/sandbox key (future)
```

Prefix `mol_pk_` makes keys easily identifiable in logs, .env files, and
secret scanners. The `live_`/`test_` segment enables environment separation
when we add sandbox mode.

Keys are 44 characters total: `mol_pk_live_` (12) + 32 hex = 44 chars.
Displayed once at creation; stored as SHA-256 hash (irreversible).

## Database schema

```sql
-- Migration: 0XX_partner_api_keys.up.sql

CREATE TABLE IF NOT EXISTS partner_api_keys (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name        TEXT        NOT NULL,                    -- "Acme Reseller", "CI Pipeline"
  key_hash    TEXT        NOT NULL UNIQUE,             -- SHA-256 of the full key
  key_prefix  TEXT        NOT NULL,                    -- "mol_pk_live_a1b2" (first 16 chars, for identification)
  partner_id  TEXT        NOT NULL,                    -- external partner identifier
  org_id      UUID,                                    -- NULL = can manage any org; set = scoped to one org
  scopes      TEXT[]      NOT NULL DEFAULT '{}',       -- {"orgs:create","orgs:read","orgs:delete","billing:read"}
  rate_limit  INTEGER     NOT NULL DEFAULT 60,         -- requests per minute
  created_by  TEXT        NOT NULL,                    -- admin user ID who created it
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at  TIMESTAMPTZ,                             -- NULL = never expires
  revoked_at  TIMESTAMPTZ,                             -- NULL = active
  last_used_at TIMESTAMPTZ                             -- updated on each use
);

CREATE INDEX IF NOT EXISTS partner_api_keys_hash_idx ON partner_api_keys(key_hash);
CREATE INDEX IF NOT EXISTS partner_api_keys_partner_idx ON partner_api_keys(partner_id);
```

## Scopes

| Scope | Grants |
|-------|--------|
| `orgs:create` | `POST /cp/orgs` — create organizations |
| `orgs:read` | `GET /cp/orgs`, `GET /cp/orgs/:slug`, `GET /cp/orgs/:slug/instance` |
| `orgs:delete` | `DELETE /cp/orgs/:slug` — full GDPR cascade |
| `orgs:export` | `GET /cp/orgs/:slug/export` — data export |
| `billing:read` | `GET /cp/orgs/:slug/usage` |
| `billing:manage` | `POST /cp/billing/checkout`, `POST /cp/billing/portal` |
| `provision:status` | `GET /cp/orgs/:slug/provision-status` |
| `admin:keys` | `POST/DELETE /cp/admin/partner-keys` — manage other keys |

Scopes are additive. A key with `["orgs:create", "orgs:read"]` can create
and list orgs but cannot delete them or manage billing.

**Org-scoped keys:** When `org_id` is set, the key can only access that
specific org. Useful for giving a partner access to manage their own org
without seeing others. When `org_id` is NULL, the key is global (admin-level).

## API endpoints

### Create a partner key (admin only)

```
POST /cp/admin/partner-keys
Authorization: Bearer <admin-session-or-existing-admin-key>
Content-Type: application/json

{
  "name": "Acme Reseller",
  "partner_id": "partner_acme",
  "scopes": ["orgs:create", "orgs:read", "billing:read"],
  "org_id": null,
  "expires_in_days": 365,
  "rate_limit": 120
}

→ 201 Created
{
  "id": "uuid",
  "name": "Acme Reseller",
  "key": "mol_pk_live_a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6",  ← shown ONCE
  "key_prefix": "mol_pk_live_a1b2",
  "scopes": ["orgs:create", "orgs:read", "billing:read"],
  "expires_at": "2027-04-17T00:00:00Z",
  "rate_limit": 120
}
```

The full key is returned exactly once. Store it securely — we only keep the
SHA-256 hash.

### List partner keys (admin only)

```
GET /cp/admin/partner-keys

→ 200 OK
[
  {
    "id": "uuid",
    "name": "Acme Reseller",
    "key_prefix": "mol_pk_live_a1b2",
    "partner_id": "partner_acme",
    "scopes": ["orgs:create", "orgs:read"],
    "created_at": "2026-04-17T...",
    "last_used_at": "2026-04-17T...",
    "expires_at": "2027-04-17T...",
    "revoked_at": null
  }
]
```

### Revoke a partner key (admin only)

```
DELETE /cp/admin/partner-keys/:id

→ 204 No Content
```

Revocation is immediate. The key's `revoked_at` is set to `now()` and all
subsequent requests with that key return 401.

## Middleware integration

The CP's auth middleware checks in order:

```go
func Middleware(authProvider auth.Provider) gin.HandlerFunc {
    return func(c *gin.Context) {
        // 1. Check for partner API key (Bearer mol_pk_*)
        if token := bearerToken(c); strings.HasPrefix(token, "mol_pk_") {
            partner, err := validatePartnerKey(c.Request.Context(), db, token)
            if err != nil {
                c.AbortWithStatusJSON(401, gin.H{"error": "invalid API key"})
                return
            }
            // Set partner context — handlers read this instead of session
            c.Set("partner", partner)
            c.Set("auth_method", "api_key")
            c.Next()
            return
        }

        // 2. Fall back to WorkOS session (existing flow)
        session := authProvider.GetSession(c)
        // ... existing session logic
    }
}
```

Handlers that need the caller's identity read from context:

```go
// Works for both session and API key callers
func getCallerID(c *gin.Context) string {
    if p, ok := c.Get("partner"); ok {
        return p.(*PartnerKey).PartnerID
    }
    if s := auth.FromContext(c.Request.Context()); s != nil {
        return s.UserID
    }
    return ""
}
```

## Rate limiting

Partner keys have per-key rate limits (default: 60 req/min, configurable).
Separate from the session-based rate limiter so partner traffic doesn't
compete with browser users.

```go
// In middleware, after validating the key:
if !rateLimiter.Allow(partner.ID, partner.RateLimit) {
    c.AbortWithStatusJSON(429, gin.H{
        "error": "rate limit exceeded",
        "retry_after": rateLimiter.RetryAfter(partner.ID),
    })
    return
}
```

## Security considerations

1. **Key storage:** SHA-256 hash only in DB. Full key shown once at creation.
2. **Key rotation:** Create new key → update partner config → revoke old key.
   No "update" endpoint — always create-then-revoke for clean audit trail.
3. **Scope enforcement:** Each handler checks required scope before executing.
   Missing scope → 403 Forbidden (not 401 — the key is valid, just not
   authorized for this action).
4. **Org isolation:** Org-scoped keys cannot access other orgs. Global keys
   (org_id=NULL) are admin-level — issue sparingly.
5. **Audit trail:** `last_used_at` updated on each use. `created_by` tracks
   who issued the key. Full request logging includes `partner_id`.
6. **Expiration:** Optional `expires_at`. Expired keys return 401 with a
   clear message ("API key expired").
7. **Pre-commit hook:** The `mol_pk_` prefix is added to the secret scanner
   pattern in `.githooks/pre-commit` to prevent accidental commits.

## Use cases

### Partner platform integration
```bash
# Partner creates an org for their customer
curl -X POST https://api.moleculesai.app/cp/orgs \
  -H "Authorization: Bearer mol_pk_live_a1b2c3d4..." \
  -H "Content-Type: application/json" \
  -d '{"slug": "customer-xyz", "name": "Customer XYZ", "plan": "starter"}'

# Partner polls provisioning status
curl https://api.moleculesai.app/cp/orgs/customer-xyz/provision-status \
  -H "Authorization: Bearer mol_pk_live_a1b2c3d4..."

# Partner checks usage for billing
curl https://api.moleculesai.app/cp/orgs/customer-xyz/usage \
  -H "Authorization: Bearer mol_pk_live_a1b2c3d4..."
```

### CI/CD testing
```bash
# Create test org, run tests, delete
ORG=$(curl -s -X POST .../cp/orgs -H "Authorization: Bearer $MOL_API_KEY" \
  -d '{"slug":"ci-test-'$GITHUB_RUN_ID'","name":"CI Test"}' | jq -r .slug)

# ... run E2E tests against $ORG.moleculesai.app ...

curl -X DELETE .../cp/orgs/$ORG -H "Authorization: Bearer $MOL_API_KEY"
```

### Internal automation (what Claude Code needs for testing)
```bash
# CEO's assistant agent creates test orgs programmatically
curl -X POST https://api.moleculesai.app/cp/orgs \
  -H "Authorization: Bearer mol_pk_live_internal..." \
  -d '{"slug":"autotest1","name":"Auto Test 1"}'
```

## Implementation order

1. **Migration:** `partner_api_keys` table
2. **Middleware:** Partner key validation in `auth.Middleware`
3. **Admin endpoints:** Create, list, revoke keys
4. **Scope enforcement:** Per-handler scope checks
5. **Rate limiter:** Per-key rate limiting
6. **Pre-commit hook:** Add `mol_pk_` to secret scanner
7. **Docs:** API reference + partner onboarding guide

## Files to change

| File | Change |
|------|--------|
| `internal/migrations/0XX_partner_api_keys.up.sql` | New table |
| `internal/auth/middleware.go` | Add partner key check before session |
| `internal/auth/partner_keys.go` | Key validation, hashing, scope check |
| `internal/handlers/partner_keys.go` | Admin CRUD endpoints |
| `internal/router/router.go` | Wire new endpoints |
| `docs/runbooks/saas-secrets.md` | Document key management |
| `.githooks/pre-commit` | Add `mol_pk_` to secret scanner |
