# Token Management API

Workspace bearer tokens authenticate agents and API clients against the EnterOS platform. Each token is scoped to a single workspace — a token from workspace A cannot access workspace B.

## Endpoints

All endpoints are behind `WorkspaceAuth` middleware — you need an existing valid token to manage tokens for a workspace. The first token is issued during workspace registration (`POST /registry/register`).

### List Tokens

```
GET /workspaces/:id/tokens
Authorization: Bearer <token>
```

Returns non-revoked tokens for the workspace. Only metadata is returned — never the plaintext or hash.

```json
{
  "tokens": [
    {
      "id": "uuid-of-token-row",
      "prefix": "abc12345",
      "created_at": "2026-04-16T12:00:00Z",
      "last_used_at": "2026-04-16T15:30:00Z"
    }
  ],
  "count": 1
}
```

### Create Token

```
POST /workspaces/:id/tokens
Authorization: Bearer <token>
```

Mints a new token. The plaintext is returned **exactly once** — save it immediately.

```json
{
  "auth_token": "dGhpcyBpcyBhIHRlc3QgdG9rZW4...",
  "workspace_id": "ws-uuid",
  "message": "Save this token now — it cannot be retrieved again."
}
```

### Revoke Token

```
DELETE /workspaces/:id/tokens/:tokenId
Authorization: Bearer <token>
```

Revokes a specific token by its database ID (from the List response). The token is immediately invalidated.

```json
{
  "status": "revoked"
}
```

Returns 404 if the token doesn't exist, belongs to a different workspace, or is already revoked.

## Token Lifecycle

```
Issue (register or POST /tokens)
  → Active (used via Authorization: Bearer)
  → Revoked (DELETE /tokens/:id or workspace deleted)
```

- Tokens have no expiration — they remain valid until explicitly revoked or the workspace is deleted
- On workspace deletion, all tokens are automatically revoked
- Multiple tokens can exist simultaneously per workspace (for rotation)

## Token Rotation

To rotate credentials without downtime:

1. **Create** a new token: `POST /workspaces/:id/tokens`
2. **Update** your agent to use the new token
3. **Verify** the new token works (check `last_used_at` in List)
4. **Revoke** the old token: `DELETE /workspaces/:id/tokens/:oldTokenId`

## Security Properties

- **256-bit entropy**: Tokens are 32 random bytes, base64url-encoded (43 characters)
- **Hash-only storage**: Only `sha256(token)` is stored in the database — plaintext is never persisted
- **Workspace-scoped**: Token from workspace A cannot authenticate as workspace B
- **One-time display**: Plaintext returned only at creation — not recoverable from the database
- **Prefix for identification**: First 8 characters stored for log correlation without revealing the token

## Bootstrap: Getting Your First Token

The first token is issued during workspace registration:

```bash
# 1. Create workspace
curl -X POST http://localhost:8080/workspaces \
  -H "Content-Type: application/json" \
  -d '{"name": "My Agent", "tier": 2}'

# 2. Register (returns auth_token)
curl -X POST http://localhost:8080/registry/register \
  -H "Content-Type: application/json" \
  -d '{"workspace_id": "<id>", "url": "http://...", "agent_card": {...}}'
# Response: {"auth_token": "...", ...}
```

Tenant admins can mint a real workspace token through the production-safe admin route:
```bash
curl -X POST http://localhost:8080/admin/workspaces/<id>/tokens \
  -H "Authorization: Bearer <ADMIN_TOKEN>"
# Response: {"auth_token": "...", "workspace_id": "..."}
```
