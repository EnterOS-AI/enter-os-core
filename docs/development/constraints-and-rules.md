# Constraints & Rules

Key design rules and invariants that must be followed throughout the codebase.

## 1. The Platform Never Routes Agent Messages

A2A messages go workspace-to-workspace **directly**. The platform only handles:
- **Discovery** — resolving workspace URLs
- **Registry** — knowing what workspaces exist

## 2. Postgres Is Source of Truth, Redis Is Ephemeral

If Redis is wiped, workspaces re-register on next heartbeat and state is restored. Nothing critical lives only in Redis.

## 3. structure_events Is Append-Only

Never `UPDATE` or `DELETE` rows in this table. The `workspaces` table is the mutable projection. See [Event Log](../architecture/event-log.md).

## 4. workspace-template Is Generic

It contains **no business logic**. All business logic lives in `workspace-configs-templates/`. The template reads config files at startup — it does not know what kind of workspace it is until it loads config.

## 5. Bundles Do Not Contain Secrets

API keys, passwords, and credentials are **never** serialized into bundle JSON. The provisioner injects them from the host environment when spinning up a workspace container.

## 6. No Auth for MVP

The platform API has no authentication. All endpoints are open. This is intentional — the project follows the n8n Community Edition model. Auth is added in a separate SaaS wrapper (`molecule-cloud`).

## 7. org_id Is Omitted from MVP Schema

Removed entirely for MVP simplicity. Added in the SaaS migration for multi-tenancy.

## 8. Tier Determines Provisioner, Not Behavior

The workspace code is the same regardless of tier. The tier only affects how the workspace is deployed:
- Docker flags
- host access level and isolation boundary
- Resource allocation

See [Workspace Tiers](../architecture/workspace-tiers.md).

## 9. The Hierarchy IS the Topology

There is no manual connection wiring. Communication is derived from the `parent_id` hierarchy:
- Siblings can talk to each other
- Parents can talk to children (and vice versa)
- No skipping levels

The org chart IS the access control policy. See [Communication Rules](../api-protocol/communication-rules.md).

## 10. Discovery-Time Auth for MVP

Direct A2A calls between workspaces are unauthenticated in MVP. Access control is enforced at discovery time via `CanCommunicate()`. Post-MVP adds platform-issued signed tokens scoped to caller/target pairs. See [A2A Protocol — Authentication](../api-protocol/a2a-protocol.md#authentication-between-workspaces).

## 11. Secrets in Postgres, Encrypted

Workspace secrets (API keys, credentials) are stored in Postgres with AES-256-GCM encryption at the application layer. The tenant's `SECRETS_ENCRYPTION_KEY` is provisioned at boot by the control plane, which holds the master key material in AWS KMS (envelope encryption, dual-mode with a static-key fallback for dev). Full custody chain in [secrets-key-custody.md](../architecture/secrets-key-custody.md). Secrets are never included in bundles, never logged, never exposed via API responses.

## 12. Last-Write-Wins for MVP

Concurrent canvas modifications from multiple clients use last-write-wins. No optimistic locking or CRDTs for MVP.

## 13. Security Headers on All Responses

The platform applies HTTP security headers via middleware (`workspace-server/internal/middleware/securityheaders.go`):
- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `X-XSS-Protection: 1; mode=block`

These are applied after CORS middleware on every response.

## 14. No Exposed Database Ports

Postgres and Redis must not expose host ports. They communicate exclusively over the internal Docker network (`molecule-core-net`). Use `docker compose exec` for direct access during development.

## Related Docs

- [SaaS Upgrade Path](../product/saas-upgrade.md) — How auth and multi-tenancy are added
- [Bundle System](../agent-runtime/bundle-system.md) — Why bundles exclude secrets
- [Event Log](../architecture/event-log.md) — The append-only rule
- [Communication Rules](../api-protocol/communication-rules.md) — Hierarchy = access control
