---
title: "Secure by Design — Molecule AI's Beta Auth Hardening Push"
date: 2026-04-20
slug: beta-auth-hardening
description: "Today's launch hardens Molecule AI's multi-tenant architecture across four dimensions: org-scoped API keys, browser session auth, tenant provisioning security, and a waitlist gate. Here's what changed and why."
tags: [security, platform, multi-tenant, auth, launch]
---

# Secure by Design — Molecule AI's Beta Auth Hardening Push

Four PR chains merged today. Together they close a week's worth of security gaps, eliminate shared secret sprawl, and put Molecule AI's beta on a production-grade auth footing. This post explains each piece and what it means for you.

---

## 1. Org-scoped API keys — full admin access without a browser

The biggest user-facing change: every Molecule AI org can now mint named, revocable bearer tokens from the Canvas Settings panel. No more copying the bootstrap `ADMIN_TOKEN` into scripts, CI pipelines, or Zapier integrations.

**What you get:**
- One key per integration — `zapier-integration`, `github-actions-deploy`, `my-claude-agent`
- Revocation is immediate: `DELETE /org/tokens/:id` returns 401 on the next request
- Every action is audited: server logs, DB `created_by`, and activity log entries carry the 8-character key prefix (`org-token:<prefix>`)
- Org keys reach every workspace in your org, including workspace sub-routes: `/workspaces`, `/workspaces/:id/channels`, `/workspaces/:id/audit`
- 10 mints per hour per IP rate limit on `POST /org/tokens` — a compromised key can't mint a flood

**The visual proof point:** Unlike CrewAI and Hermes (user-prefixed keys), a Molecule org key shows `org:abc123XY` in the admin UI — the org prefix is visible in server logs, every audit row, and the token list. Trivial correlation, full auditability.

→ [User guide: Organization API Keys](/docs/guides/org-api-keys.md)
→ [Architecture: Org API Keys](/docs/architecture/org-api-keys.md)
→ PRs: [#1105](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1105), [#1107](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1107), [#1109](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1109), [#1110](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1110)

---

## 2. Browser session auth — Canvas admins don't need bearer tokens

Canvas runs in the browser and authenticates users via a WorkOS session cookie (scoped to `.moleculesai.app`). It had no bearer token — which meant the platform couldn't recognize Canvas admin sessions as equivalent to an `ADMIN_TOKEN` bearer.

AdminAuth now accepts a session-verification tier that runs **before** the bearer check:

1. Canvas browser sends the WorkOS session cookie to any admin-routed endpoint
2. The tenant platform calls `GET /cp/auth/tenant-member?slug=<your-tenant>` upstream with the same cookie
3. 200 + `member: true` → grant admin access; non-200 or no cookie → fall through to bearer path

**The security constraint that makes this safe:** the verification call includes the tenant slug and checks that the session belongs to a *member of this specific tenant*, not just "someone logged in to moleculesai.app." A session scoped to a different tenant's org fails the check.

**Caching:** positive results cached 30 seconds (keyed `sha256(slug + cookie)`); negative results cached 5 seconds. Revocations propagate within that window. No thundering herd on CP when a burst of Canvas admin pages render.

**Self-hosted / local dev:** `CP_UPSTREAM_URL` is unset → this feature is disabled, behaviour is unchanged.

→ [Guide: Same-Origin Canvas Fetches & Session Auth](/docs/guides/same-origin-canvas-fetches.md)
→ PRs: [#1099](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1099), [#1100](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1100)

---

## 3. Tenant provisioning security — structural fixes, not policy patches

The tenant provisioning work closed several credential and isolation gaps that existed in the multi-tenant bootstrap path:

**Secrets manager:** `PutSecret` now creates the secret before any update, fixing a race where a failed intermediate step left a partial credential state.

**IAM policy gaps:** The control plane's IAM role needed `secretsmanager:*`, `iam:PassRole`, and `ec2:GetConsoleOutput` to complete workspace boot cleanly. These are now present.

**Boot observability:** A new boot-event phone-home channel lets operators observe tenant startup from inside the platform rather than inferring state from external probes.

**Cross-tenant isolation:** Two gaps closed:
- `TenantGuard` now pass-through correctly for `/cp/*` proxy routes — a tenant can't forge requests on behalf of another tenant through the CP proxy.
- `X-Molecule-Org-Id` header validation hardened so cross-tenant reads are structurally blocked before they reach any handler.

→ Architecture docs in the control plane repo

---

## 4. Same-origin canvas fetches — /cp/* proxy removes cross-origin complexity

Canvas's browser bundle needs to call both the tenant platform (for workspace management) and the control plane (for org operations, billing, session verification). Before today, that meant two separate base URLs in the browser build, CORS preflights on CP calls, and cookie domain complications.

The fix: the tenant platform now runs a `/cp/*` reverse proxy. Canvas makes all calls to its single `NEXT_PUBLIC_PLATFORM_URL` (the tenant). The tenant splits the traffic server-side:

```
Browser → tenant.moleculesai.app
  ├── /workspaces, /approvals/pending  → handled locally
  └── /cp/*                            → reverse-proxied upstream to CP
```

The proxy is **fail-closed**: only an explicit allowlist of paths (`/cp/auth/`, `/cp/orgs`, `/cp/billing/`, `/cp/templates`, `/cp/legal/`) is forwarded. Any other `/cp/*` path returns 404 — not 403 — to avoid leaking which CP routes exist.

This is also the structural fix for the lateral-movement risk that session auth introduced: without the allowlist, a tenant-authed browser user could have proxied `/cp/admin/*` requests upstream and exploited the fact that those endpoints accept WorkOS session cookies. The allowlist makes that impossible by construction.

→ [Guide: Same-Origin Canvas Fetches & Session Auth](/docs/guides/same-origin-canvas-fetches.md)
→ PR: [#1095](https://git.moleculesai.app/molecule-ai/molecule-core/pull/1095)

---

## 5. Beta gate + waitlist — controlled rollout for the waitlist cohort

Canvas now gates unauthenticated visitors on the `/cp/auth/tenant-member` route — a request that verifies the user is a member of an approved org before any workspace data is served. Non-members hit a waitlist contact form instead.

The waitlist itself is a Canvas-administered list with email hashing in audit logs (compliant with EU AI Act record-keeping requirements). Admins triage signups from an internal UI.

This is the operational surface that makes the above security work matter: the beta is invitation-only, credentials are scoped, and every admin action is auditable.

→ Control plane PRs [#145](https://git.moleculesai.app/molecule-ai/molecule-controlplane/pull/145), [#148](https://git.moleculesai.app/molecule-ai/molecule-controlplane/pull/148), [#150](https://git.moleculesai.app/molecule-ai/molecule-controlplane/pull/150)

---

## What this means in practice

If you're already using Molecule AI as a self-hosted deployment, nothing changes today — the auth tier improvements are SaaS-only until you opt into multi-tenant mode.

If you're on the beta waitlist, you'll receive an invite. Once onboarded, your Canvas session is your admin credential. Mint org API keys for your scripts and integrations. Revoke them if anything looks wrong.

If you're evaluating Molecule AI: this launch marks the point where the platform's security posture is intentional and documented, not accumulated accident. Org keys, session auth, and tenant isolation are all covered in the architecture docs — not just the marketing claims.

→ [Quickstart](/docs/quickstart)
→ [Architecture overview](/docs/architecture/architecture)
→ [Platform API reference](/docs/api-reference)

---

*PRs #1075–#1083, #1085–#1100 (monorepo), #145–#150, #153–#169, #172–#173 (controlplane), #12 (molecule-app). Production rollout on 2026-04-20.*
