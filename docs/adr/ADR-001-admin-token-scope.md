# ADR-001: Admin endpoints accept any workspace bearer token

**Status:** Accepted — known risk, Phase-H remediation planned
**Date:** 2026-04-17
**Issue:** #684
**Tracking:** Phase-H — #710

## Context

The `AdminAuth` middleware validates callers by calling `ValidateAnyToken`, which
accepts any live workspace bearer token regardless of which workspace issued it.
There is no separation between workspace-scoped tokens (issued to individual
agents) and admin-scoped tokens (intended for platform operators).

This means any workspace agent that has been issued a token can reach every
admin-gated route on the platform.

## Decision

Proper token-tier separation (workspace vs. admin scope) is deferred to Phase-H.
The known risk is explicitly accepted. Mitigation controls are documented below.

## Blast radius — affected admin endpoints

A compromised workspace token grants unauthenticated-equivalent access to all
of the following:

| Endpoint | Impact |
|----------|--------|
| `POST /admin/workspaces/:id/tokens` | Mint a fresh real bearer token for any workspace |
| `DELETE /workspaces/:id` | Delete any workspace and auto-revoke its tokens |
| `PUT /settings/secrets` / `POST /admin/secrets` | Overwrite any global secret (env-poisons every agent on restart) |
| `DELETE /settings/secrets/:key` / `DELETE /admin/secrets/:key` | Delete any global secret; same fan-out restart |
| `GET /settings/secrets` / `GET /admin/secrets` | Read all global secret keys (values masked, but key enumeration enables targeted attacks) |
| `GET /workspaces/:id/budget` + `PATCH /workspaces/:id/budget` | Read or clear any workspace's token budget |
| `GET /events` / `GET /events/:workspaceId` | Read the full structural event log across all workspaces |
| `POST /bundles/import` | Import an arbitrary workspace bundle — creates workspaces, injects secrets, overwrites configs |
| `GET /bundles/export/:id` | Exfiltrate full workspace bundle including config, secrets references, and files |
| `POST /org/import` | Instantiate an entire org template — creates multiple workspaces with arbitrary roles and secrets |
| `GET /org/templates` | Enumerate all org template names and their configured roles/system prompts |
| `POST /templates/import` | Write arbitrary files into `configsDir` (workspace template injection) |
| `GET /templates` | Enumerate all template names and metadata |
| `GET /admin/liveness` | Read platform subsystem health (ops intel) |
| `GET /admin/schedules/health` | Read cron scheduler health across all workspaces |

## Risk statement

**A single compromised workspace agent can achieve full platform takeover via
admin endpoints.**

Attack chain example:
1. Agent A's token is exfiltrated (e.g. via a prompt-injection in a delegated task).
2. Attacker calls `PUT /settings/secrets` to overwrite `CLAUDE_API_KEY` with a
   controlled value.
3. Every non-paused workspace restarts and loads the poisoned key.
4. Attacker now controls the LLM backend for the entire platform.

Alternatively: call `POST /bundles/import` with a crafted bundle to inject a
malicious workspace with a pre-configured `initial_prompt` and elevated secrets.

## Current mitigations

- **Workspace isolation** — `CanCommunicate()` in the A2A proxy limits which
  workspaces can send tasks to which, reducing the blast radius of a single
  compromised agent during normal operation.
- **Audit logging** — PR #651 writes all admin-route calls to `structure_events`.
  Forensic recovery is possible after the fact.
- **`ValidateAnyToken` removed-workspace JOIN** — tokens belonging to deleted
  workspaces are filtered at the DB layer (PR #682 defense-in-depth) so
  post-deletion token replay is blocked.
- **Production token mint route** — production and staging automation use
  `POST /admin/workspaces/:id/tokens`; development-only shortcuts are not part
  of the production contract.

## Phase-H remediation plan

Tracked in GitHub issue **#710**.

### Schema change

Add a `token_type` column to `workspace_auth_tokens`:

```sql
ALTER TABLE workspace_auth_tokens
  ADD COLUMN IF NOT EXISTS token_type TEXT NOT NULL DEFAULT 'workspace'
  CHECK (token_type IN ('workspace', 'admin'));
```

Admin tokens are minted only via a dedicated privileged endpoint that itself
requires an existing admin token or a one-time bootstrap secret.

### Middleware update

- `WorkspaceAuth` — continue accepting `token_type = 'workspace'` only.
- `AdminAuth` — require `token_type = 'admin'`. Workspace tokens rejected.

### Bootstrap flow

On first boot (no tokens exist), a single-use bootstrap secret is printed to
the server log. The operator uses it to mint the first admin token. Subsequent
admin tokens are minted by existing admin token holders. The fail-open path in
`HasAnyLiveTokenGlobal` is retired once Phase-H ships.

### Migration path

Phase-H is a breaking change for any automation that currently uses workspace
tokens against admin endpoints. A migration guide and a `MOLECULE_PHASE_H=1`
feature flag will be provided so operators can opt in before the strict
enforcement date.
