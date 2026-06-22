---
title: "Ship Partner Integrations Faster with Programmatic Org Management"
date: 2026-04-23
slug: partner-api-keys
description: "Partner API Keys let marketplace resellers, CI/CD pipelines, and automation tools create and manage EnterOS orgs via API — no browser session required."
og_title: "Ship Partner Integrations Faster with Programmatic Org Management"
og_description: "Partner API Keys: scoped, rate-limited, revocable API keys for programmatic org management. Built for marketplaces, CI/CD, and automation platforms."
tags: [partner-api-keys, marketplace, ci-cd, automation, api, enterprise, provisioning]
keywords: [partner API keys, programmatic org management, marketplace integration, CI/CD automation, EnterOS API, reseller integration, org provisioning API]
canonical: https://docs.molecule.ai/blog/partner-api-keys
---

<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "Ship Partner Integrations Faster with Programmatic Org Management",
  "description": "Partner API Keys let marketplace resellers, CI/CD pipelines, and automation tools create and manage EnterOS orgs via API.",
  "author": { "@type": "Organization", "name": "EnterOS" },
  "datePublished": "2026-04-23",
  "publisher": { "@type": "Organization", "name": "EnterOS", "logo": { "@type": "ImageObject", "url": "https://molecule.ai/logo.png" } }
}
</script>

# Ship Partner Integrations Faster with Programmatic Org Management

When your platform needs to create an org — for a new customer, a CI environment, or a marketplace resale — the last thing you want is to hand that flow over to a human with a browser. Neither does your partner.

Phase 34 is designed to solve exactly this problem. **Partner API Keys** give marketplace resellers, CI/CD pipelines, and automation platforms a programmatic way to create and manage EnterOS orgs — no browser session, no admin dashboard, just an API call.

## What Partner API Keys Do

A Partner API Key is a scoped, rate-limited, revocable bearer token — prefixed `mol_pk_` — that lives at the `/cp/` control plane boundary. It authenticates to a set of partner-facing endpoints that let you provision an org, poll its status, and revoke the integration when it's no longer needed.

Unlike org-scoped API keys (which operate *within* an org), Partner API Keys operate *at the org level*: they create orgs, list your own keys, and revoke themselves. The scope system lets you grant exactly the capabilities a partner needs — nothing more.

```bash
POST /cp/admin/partner-keys
Authorization: Bearer <admin-master-key>
{
  "name": "acme-ci-pipeline",
  "scopes": ["orgs:create", "orgs:list"],
  "org_id": null
}

# Response
{
  "id": "pak_01HXKM4...",
  "key": "mol_pk_1a2b3c4d5e...",   # shown ONCE
  "name": "acme-ci-pipeline",
  "scopes": ["orgs:create", "orgs:list"],
  "created_at": "2026-04-23T08:00:00Z"
}
```

Your CI pipeline saves `mol_pk_1a2b3c4d5e...` as a secret and uses it to call the partner endpoints.

## The Partner API Surface

Once you have a Partner API Key, the integration flow looks like this:

```bash
# 1. Create an org
POST /cp/orgs
Authorization: Bearer mol_pk_1a2b3c4d5e...
{
  "name": "acme-corp",
  "slug": "acme-corp",
  "plan": "standard"
}

# Response
{
  "id": "org_01HXKM4...",
  "slug": "acme-corp",
  "status": "provisioning",
  "created_at": "2026-04-23T08:00:00Z"
}

# 2. Poll until ready
GET /cp/orgs/org_01HXKM4.../status
Authorization: Bearer mol_pk_1a2b3c4d5e...

# 3. Redirect the customer
# → https://app.moleculesai.app/login?org=acme-corp

# 4. Revoke when done
DELETE /cp/admin/partner-keys/pak_01HXKM4...
Authorization: Bearer mol_pk_1a2b3c4d5e...
```

Every call is audited: the audit log records which Partner API Key was used, when, and what it did — so you can trace a provisioning event back to the integration that triggered it.

## Scopes and Rate Limits

Partner API Keys are granted specific scopes at creation time. A CI pipeline might get `orgs:create` + `orgs:list`. A marketplace reseller might also need `workspaces:create`. A monitoring tool might only need `orgs:list`.

```
Available scopes:
  orgs:create      — provision new orgs
  orgs:list        — list partner-managed orgs
  orgs:delete      — deprovision orgs
  workspaces:create — create workspaces within an org
  billing:read     — read subscription status
```

Rate limits are enforced per key, independently of the session rate limit. A misbehaving integration hits its own ceiling without affecting other partners or organic traffic.

## The Marketplace Reseller Use Case

Marketplace resellers need to provision a EnterOS org on behalf of every end customer — automatically, at scale, without a human in the loop. They also need to:

- **Scope the integration** to only the capabilities that partner needs
- **Revoke cleanly** when the reseller-customer relationship ends
- **Audit everything** for compliance reporting

Partner API Keys handle all three. A reseller creates one key per integration tier (e.g. one key for the standard tier, one for enterprise), each scoped to exactly what that tier allows. When a customer churns, the reseller revokes their key — the org stays but the automation path is closed.

## CI/CD: Ephemeral Test Orgs

CI/CD pipelines benefit from the same pattern. A test suite that needs to validate the EnterOS integration flow can:

1. Create a temporary org via Partner API Key (`orgs:create`)
2. Run the integration tests against it
3. Delete the org when done (`orgs:delete`)
4. Revoke the key

Each run gets a clean environment. No shared state, no test pollution, no manual cleanup.

## Get Started

Partner API Keys are available on **Partner and Enterprise plans**. To get started:

- Contact your account team to request Partner API Key issuance
- Review the partner integration guide (coming soon)
- Example flows: create org → poll status → redirect to tenant; CI/CD test org lifecycle

---

*EnterOS is open source. Partner API Keys shipped in Phase 34 (2026-04-23). Available on Partner and Enterprise plans.*
