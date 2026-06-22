/**
 * Playwright global teardown — deletes the staging org provisioned by
 * staging-setup.ts via DELETE /cp/admin/tenants/:slug. Runs on success AND
 * failure (Playwright calls globalTeardown regardless).
 *
 * The workflow's always()-step safety net also catches orphan orgs
 * tagged with the run ID, so this is the primary cleanup and the
 * workflow step is the belt-and-braces backup.
 */

import { existsSync, readFileSync, unlinkSync } from "fs";
import { join } from "path";

const CP_URL = process.env.MOLECULE_CP_URL || "https://staging-api.moleculesai.app";
const ADMIN_TOKEN = process.env.MOLECULE_ADMIN_TOKEN;
const STAGING = process.env.CANVAS_E2E_STAGING === "1";

export default async function globalTeardown(): Promise<void> {
  if (!STAGING) return;
  if (!ADMIN_TOKEN) {
    console.warn("[staging-teardown] no MOLECULE_ADMIN_TOKEN, skipping");
    return;
  }

  const stateFile = join(process.cwd(), ".playwright-staging-state.json");
  if (!existsSync(stateFile)) {
    // staging-setup writes this file as its first action, before any
    // CP call. Missing here means setup never ran (CANVAS_E2E_STAGING
    // unset, or ran in a different cwd) — there's no slug we created
    // that needs cleaning up.
    console.warn("[staging-teardown] no state file — nothing to tear down");
    return;
  }

  let slug: string;
  try {
    const state = JSON.parse(readFileSync(stateFile, "utf-8"));
    slug = state.slug;
  } catch (e) {
    console.warn(`[staging-teardown] state file unreadable: ${e}`);
    return;
  }

  console.log(`[staging-teardown] Deleting org ${slug}...`);
  try {
    const res = await fetch(`${CP_URL}/cp/admin/tenants/${slug}`, {
      method: "DELETE",
      headers: {
        Authorization: `Bearer ${ADMIN_TOKEN}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ confirm: slug }),
    });
    if (res.ok) {
      console.log(`[staging-teardown] ${slug} deleted`);
    } else {
      console.warn(
        `[staging-teardown] DELETE returned ${res.status} (may already be gone)`,
      );
    }
  } catch (e) {
    console.warn(`[staging-teardown] DELETE failed: ${e}`);
  }

  try {
    unlinkSync(stateFile);
  } catch {
    /* non-fatal */
  }
}
