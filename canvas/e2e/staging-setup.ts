/**
 * Playwright global setup for the staging canvas E2E.
 *
 * Provisions a fresh staging org per run (POST /cp/admin/orgs), fetches
 * the per-tenant admin token, provisions one hermes workspace, waits
 * for online, then exports:
 *
 *   STAGING_TENANT_URL     https://<slug>.staging.moleculesai.app
 *   STAGING_WORKSPACE_ID   UUID of the hermes workspace
 *   STAGING_TENANT_TOKEN   per-tenant admin bearer (for spec requests)
 *   STAGING_SLUG           org slug (used by teardown)
 *
 * Required env:
 *   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
 *   MOLECULE_ADMIN_TOKEN   CP admin bearer (Railway staging
 *                          CP_ADMIN_API_TOKEN). Drives provision +
 *                          tenant-token retrieval + teardown via a
 *                          single credential.
 *   STAGING_TENANT_DOMAIN  default: staging.moleculesai.app — the
 *                          DNS suffix the CP provisioner writes for
 *                          staging tenants. Override only when
 *                          running this harness against a non-default
 *                          zone.
 */

import type { FullConfig } from "@playwright/test";
import { writeFileSync } from "fs";
import { join } from "path";

const CP_URL = process.env.MOLECULE_CP_URL || "https://staging-api.moleculesai.app";
const ADMIN_TOKEN = process.env.MOLECULE_ADMIN_TOKEN;
const STAGING = process.env.CANVAS_E2E_STAGING === "1";
// Tenant DNS zone for staging. CP provisioner registers DNS as
// `<slug>.staging.moleculesai.app` (see internal/provisioner/ec2.go's
// EC2 provisioner: DNS log line). The previous default of plain
// `moleculesai.app` matched prod tenant naming and silently broke
// every staging E2E at the TLS readiness step — DNS literally didn't
// resolve, fetch threw NXDOMAIN, waitFor saw null on every poll, and
// the harness wedged at TLS_TIMEOUT_MS instead of failing loud.
const TENANT_DOMAIN = process.env.STAGING_TENANT_DOMAIN || "staging.moleculesai.app";

// Tenant cold boot on staging regularly takes 12-15 min when the
// workspace-server Docker image isn't already cached on the AMI. Raised
// to 20 min to match tests/e2e/test_staging_full_saas.sh (PR #1930)
// after repeated "tenant provision: timed out after 900s" flakes
// were blocking staging→main syncs on 2026-04-24.
const PROVISION_TIMEOUT_MS = 20 * 60 * 1000;
const WORKSPACE_ONLINE_TIMEOUT_MS = 20 * 60 * 1000;

// TLS readiness depends on (1) Cloudflare DNS propagation through the
// edge, (2) the tenant's CF Tunnel registering the new hostname, (3)
// CF's edge ACME cert provisioning + cache. Each of these layers can
// add 1-3 min on its own under heavy staging load. Bumped 10→15 min
// after a burst of canary failures correlated with CP changes (#2090).
// Stays below the 20-min PROVISION_TIMEOUT envelope so a genuinely-
// stuck tenant fails-loud at the provision step rather than
// masquerading as a TLS issue. Kept aligned with
// tests/e2e/test_staging_full_saas.sh.
const TLS_TIMEOUT_MS = 15 * 60 * 1000;

async function jsonFetch(
  url: string,
  init: RequestInit = {},
): Promise<{ status: number; body: any }> {
  const res = await fetch(url, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init.headers || {}) },
  });
  let body: any = null;
  try {
    body = await res.json();
  } catch {
    /* non-JSON */
  }
  return { status: res.status, body };
}

async function waitFor<T>(
  op: () => Promise<T | null>,
  deadlineMs: number,
  intervalMs: number,
  desc: string,
): Promise<T> {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    const v = await op();
    if (v !== null) return v;
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  throw new Error(`${desc}: timed out after ${Math.round(deadlineMs / 1000)}s`);
}

function makeSlug(): string {
  const y = new Date().toISOString().slice(0, 10).replace(/-/g, "");
  const rand = Math.random().toString(36).slice(2, 8);
  return `e2e-canvas-${y}-${rand}`.slice(0, 32);
}

export default async function globalSetup(_config: FullConfig): Promise<void> {
  if (!STAGING) {
    console.log("[staging-setup] CANVAS_E2E_STAGING not set, skipping");
    return;
  }
  if (!ADMIN_TOKEN) {
    throw new Error(
      "MOLECULE_ADMIN_TOKEN required (Railway staging CP_ADMIN_API_TOKEN)",
    );
  }

  const slug = makeSlug();
  const adminAuth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
  console.log(`[staging-setup] Using slug=${slug}`);

  // Write the state file FIRST, before any CP call. Teardown (both
  // Playwright globalTeardown and the workflow safety-net) reads this
  // file to identify the slug it must clean up. If we wait until the
  // end of setup to write it (the previous behavior), a crash during
  // any of steps 1-6 leaves the org orphaned in CP with no record on
  // disk — forcing the workflow safety-net into a pattern-sweep over
  // every `e2e-canvas-<date>-*` org, which races with concurrent
  // canvas-E2E runs and deletes their live tenants. Race observed
  // 2026-04-30 on PR #2264 staging→main: three real-test runs killed
  // each other's tenants mid-test, surfacing as `getaddrinfo ENOTFOUND`
  // when CP cleaned up the just-deleted DNS record.
  const stateFile = join(process.cwd(), ".playwright-staging-state.json");
  writeFileSync(stateFile, JSON.stringify({ slug }, null, 2));

  // 1. Create org via admin endpoint — no WorkOS session needed
  const create = await jsonFetch(`${CP_URL}/cp/admin/orgs`, {
    method: "POST",
    headers: adminAuth,
    body: JSON.stringify({
      slug,
      name: `E2E Canvas ${slug}`,
      owner_user_id: `e2e-runner:${slug}`,
    }),
  });
  if (create.status >= 400) {
    throw new Error(
      `POST /cp/admin/orgs ${create.status}: ${JSON.stringify(create.body)}`,
    );
  }
  console.log(`[staging-setup] Org created: ${slug}`);

  // 2. Wait for tenant running (admin-orgs list is the status source).
  //
  // The CP /cp/admin/orgs endpoint returns each org with an
  // `instance_status` field (handlers/admin.go:adminOrgSummary,
  // sourced from `org_instances.status`). NOT `status` — there's no
  // top-level `status` on the row at all. A previous version of this
  // test polled `row.status`, which was always undefined, so this
  // waitFor never resolved truthy and the harness invariably timed
  // out at 1200s — masking real CP bugs (see #242 chain) AND
  // surviving real CP fixes alike.
  // Capture the org UUID alongside the running check — every request
  // we send to the tenant URL after this point needs an
  // X-Molecule-Org-Id header (see workspace-server middleware/tenant_guard.go).
  // Without it, TenantGuard returns 404 ("must not be inferable by
  // probing other orgs' machines"). The CP returns the id on the
  // admin-orgs row; capture it here while we're already polling.
  let orgID = "";
  await waitFor<boolean>(
    async () => {
      const r = await jsonFetch(`${CP_URL}/cp/admin/orgs`, { headers: adminAuth });
      if (r.status !== 200) return null;
      const row = (r.body?.orgs || []).find((o: any) => o.slug === slug);
      if (!row) return null;
      if (row.instance_status === "running") {
        orgID = row.id;
        return true;
      }
      if (row.instance_status === "failed") {
        // Dump every diagnostic field the admin row carries — boot stage,
        // last error, terraform/SSM state, etc. The bare slug message used
        // to surface ZERO context, so triaging a failed provision meant
        // re-running locally to repro. Now the failure log carries enough
        // to point at the right subsystem (CP/AWS/SSM/runtime) without a
        // second round-trip.
        throw new Error(
          `provision failed: ${slug} — admin-orgs row: ${JSON.stringify(row)}`,
        );
      }
      return null;
    },
    PROVISION_TIMEOUT_MS,
    15_000,
    "tenant provision",
  );
  if (!orgID) {
    throw new Error(`expected admin-orgs row to carry id, got empty for slug=${slug}`);
  }
  console.log(`[staging-setup] Tenant running (org_id=${orgID})`);

  // 3. Fetch per-tenant admin token
  const tokRes = await jsonFetch(
    `${CP_URL}/cp/admin/orgs/${slug}/admin-token`,
    { headers: adminAuth },
  );
  if (tokRes.status !== 200 || !tokRes.body?.admin_token) {
    throw new Error(
      `tenant-token fetch ${tokRes.status}: ${JSON.stringify(tokRes.body)}`,
    );
  }
  const tenantToken: string = tokRes.body.admin_token;
  const tenantURL = `https://${slug}.${TENANT_DOMAIN}`;
  console.log(`[staging-setup] Tenant URL: ${tenantURL}`);

  // 4. TLS readiness
  await waitFor<boolean>(
    async () => {
      try {
        const res = await fetch(`${tenantURL}/health`, {
          signal: AbortSignal.timeout(5000),
        });
        return res.ok ? true : null;
      } catch {
        return null;
      }
    },
    TLS_TIMEOUT_MS,
    5_000,
    "tenant TLS",
  );

  // 5. Provision workspace
  //
  // tenantAuth carries TWO headers, both required:
  //   - Authorization: Bearer <admin-token>  — wsAdmin middleware gate
  //   - X-Molecule-Org-Id: <uuid>           — TenantGuard cross-org gate
  // Missing the org-id header silently 404s every non-allowlisted
  // route, with no body and no security headers. The 404 is intentional
  // (existence-non-inference) which makes it look like a missing route.
  const tenantAuth = {
    "Authorization": `Bearer ${tenantToken}`,
    "X-Molecule-Org-Id": orgID,
  };
  const ws = await jsonFetch(`${tenantURL}/workspaces`, {
    method: "POST",
    headers: tenantAuth,
    body: JSON.stringify({
      name: "E2E Canvas Test",
      runtime: "hermes",
      tier: 2,
      model: "gpt-4o",
    }),
  });
  if (ws.status >= 400 || !ws.body?.id) {
    throw new Error(`Workspace create ${ws.status}: ${JSON.stringify(ws.body)}`);
  }
  const workspaceId = ws.body.id as string;
  console.log(`[staging-setup] Workspace created: ${workspaceId}`);

  // 6. Wait for workspace online
  await waitFor<boolean>(
    async () => {
      const r = await jsonFetch(`${tenantURL}/workspaces/${workspaceId}`, {
        headers: tenantAuth,
      });
      if (r.status !== 200) return null;
      if (r.body?.status === "online") return true;
      if (r.body?.status === "failed") {
        // last_sample_error is often empty when the failure happens before
        // the agent emits a sample (e.g. boot crash, image pull error,
        // missing PYTHONPATH, OpenAI quota at startup). Dumping the full
        // body gives triage the boot_stage / last_error / image fields it
        // needs without a second probe. Otherwise this propagates as a
        // bare "Workspace failed: " — the exact useless message that
        // sent #2632 to the issue tracker.
        const detail = r.body.last_sample_error
          ? r.body.last_sample_error
          : `(no last_sample_error) full body: ${JSON.stringify(r.body)}`;
        throw new Error(`Workspace failed: ${detail}`);
      }
      return null;
    },
    WORKSPACE_ONLINE_TIMEOUT_MS,
    10_000,
    "workspace online",
  );
  console.log(`[staging-setup] Workspace online`);

  // 7. Hand state off to tests + teardown — overwrite the slug-only
  // bootstrap state with the full state spec tests need.
  writeFileSync(
    stateFile,
    JSON.stringify({ slug, tenantURL, workspaceId, tenantToken }, null, 2),
  );
  process.env.STAGING_SLUG = slug;
  process.env.STAGING_TENANT_URL = tenantURL;
  process.env.STAGING_WORKSPACE_ID = workspaceId;
  process.env.STAGING_TENANT_TOKEN = tenantToken;
  console.log(`[staging-setup] Ready — ${stateFile}`);
}
