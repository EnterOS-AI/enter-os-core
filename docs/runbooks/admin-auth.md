# Admin Authentication Runbook

## Test-token route: lock in staging and production

The `GET /admin/workspaces/:id/test-token` endpoint mints fresh workspace auth tokens.
It is gated by `TestTokensEnabled()` which returns `true` only when `MOLECULE_ENV != "production"`.

**Effect**: if `MOLECULE_ENV` is unset or set to `development` / `dev` in a staging or production
tenant, the test-token route remains enabled. While the route is protected by `subtle.ConstantTimeCompare`
against `ADMIN_TOKEN` (returns 404 when disabled, not 403), the safest posture is to lock it
out in any environment where it is not intentionally used.

### Required: set MOLECULE_ENV in all non-dev environments

```bash
# In your tenant / EC2 / Railway environment variables:
MOLECULE_ENV=production
```

This matches the production tenant default. When `MOLECULE_ENV=production`:

- `TestTokensEnabled()` → `false`
- `GET /admin/workspaces/:id/test-token` → 404 (route disabled)

### Startup visibility

workspace-server logs the test-token route state at boot:

```
Platform starting on ... (dev-mode-fail-open=...)
```

Additionally, when `TestTokensEnabled()` is `true` (route enabled), the server emits an INFO line
so operators can confirm the setting in logs:

```
[molecule-git-token-helper] NOTE: /admin/workspaces/:id/test-token is ENABLED
(running with MOLECULE_ENV != production)
```

If you do not see this line and the route is still accessible, verify `MOLECULE_ENV` is not set to
`development`, `dev`, or any value that is not exactly `production`.

### Dev environments

In local dev (`MOLECULE_ENV=development` or unset with no `ADMIN_TOKEN`), the test-token route
is intentionally enabled — it is the only way to bootstrap a workspace bearer token without a running
canvas. This is the correct default for developer workstations.

## Admin bearer token (`ADMIN_TOKEN`)

The platform uses `ADMIN_TOKEN` as the bearer credential for admin-gated endpoints:

| Endpoint | Auth method |
|----------|-------------|
| `GET/POST/PATCH/DELETE /workspaces` | `Authorization: Bearer <ADMIN_TOKEN>` |
| `GET /admin/liveness` | `Authorization: Bearer <ADMIN_TOKEN>` |
| `POST /org/import` | `Authorization: Bearer <ADMIN_TOKEN>` |
| `GET /admin/workspaces/:id/test-token` | `Authorization: Bearer <ADMIN_TOKEN>` (enabled only when `MOLECULE_ENV != "production"`) |

Missing or invalid `ADMIN_TOKEN` → AdminAuth fails open in dev mode (no token set), or
returns 401 in production mode (token set but invalid).
