import { test, expect, type APIRequestContext } from "@playwright/test";

/**
 * Playwright E2E for context-menu → delete confirm flow.
 * Regression test for the portal/race bug fixed in PR #1133:
 * clicking "Delete" in the context menu did nothing because the
 * portal-rendered ConfirmDialog was closed by the menu's outside-click
 * handler before onConfirm could fire.
 *
 * The fix hoists dialog state to the canvas store via `setPendingDelete`,
 * which survives ContextMenu unmount. This test exercises the full
 * interaction in a real browser environment.
 *
 * Requires: platform on :8080, canvas on :3000.
 */
const API = process.env.E2E_API_URL ?? "http://localhost:8080";

/** Create and register a leaf workspace that will render on the canvas. */
async function seedWorkspace(request: APIRequestContext, name: string) {
  const create = await request.post(`${API}/workspaces`, {
    data: { name, tier: 1, runtime: "claude-code" },
    headers: { "Content-Type": "application/json" },
  });
  const workspace = (await create.json()) as { id: string; name: string };

  await request.post(`${API}/registry/register`, {
    data: {
      id: workspace.id,
      url: `http://localhost:9999`,
      agent_card: { name, skills: [] },
    },
    headers: { "Content-Type": "application/json" },
  });

  return workspace;
}

test.describe("Context Menu → Delete Confirm", () => {
  test("Delete button opens ConfirmDialog and clicking Confirm deletes the workspace", async ({
    page,
    request,
  }) => {
    // Fail-closed: this test seeds its own workspace and targets it by name.
    // It does NOT assume an empty canvas, and it never calls test.skip().

    // 1. Create a workspace to delete (leaf node — no children, no cascade)
    const { id: wsId } = await seedWorkspace(request, "E2E Delete Test");

    // 2. Open the canvas and wait for the workspace node
    await page.goto("/", { waitUntil: "networkidle" });
    await page.waitForTimeout(2000); // allow WS to appear

    // Find the workspace node on the canvas
    const node = page.locator(`.react-flow__node`).filter({ hasText: "E2E Delete Test" }).first();
    await expect(node).toBeVisible({ timeout: 10000 });

    // 3. Right-click to open context menu
    await node.click({ button: "right" });
    const menu = page.locator('[role="menu"]').first();
    await expect(menu).toBeVisible({ timeout: 3000 });
    await expect(menu).toHaveAttribute("aria-label", /E2E Delete Test/i);

    // 4. Click "Delete" — should open the ConfirmDialog (not close silently)
    const deleteBtn = menu.getByRole("menuitem").filter({ hasText: /Delete/i });
    await expect(deleteBtn).toBeVisible();
    await deleteBtn.click();

    // 5. ConfirmDialog should appear (portal renders into document.body)
    const dialog = page.locator('[role="dialog"]');
    await expect(dialog).toBeVisible({ timeout: 3000 });
    await expect(dialog).toContainText(/delete/i);
    await expect(dialog.getByRole("button", { name: /confirm|delete/i })).toBeVisible();

    // 6. Click Confirm — workspace should be deleted
    await dialog.getByRole("button", { name: /confirm|delete/i }).first().click();

    // 7. Dialog should close
    await expect(dialog).not.toBeVisible({ timeout: 3000 });

    // 8. Node should disappear from canvas
    await expect(
      page.locator(`.react-flow__node`).filter({ hasText: "E2E Delete Test" })
    ).not.toBeVisible({ timeout: 5000 });

    // 9. API confirms workspace is gone
    const getRes = await request.get(`${API}/workspaces/${wsId}`);
    expect(getRes.status()).toBeGreaterThanOrEqual(400); // 404 or similar
  });

  test("Cancel closes the dialog and the workspace remains", async ({ page, request }) => {
    // Seed our own workspace so this test is fail-closed and does not depend
    // on leftovers from earlier suites.
    const { name: wsName } = await seedWorkspace(request, "E2E Cancel Test");

    await page.goto("/", { waitUntil: "networkidle" });
    await page.waitForTimeout(2000);

    const node = page.locator(`.react-flow__node`).filter({ hasText: wsName }).first();
    await node.click({ button: "right" });

    const menu = page.locator('[role="menu"]').first();
    await expect(menu).toBeVisible();

    await menu.getByRole("menuitem").filter({ hasText: /Delete/i }).click();
    const dialog = page.locator('[role="dialog"]');
    await expect(dialog).toBeVisible({ timeout: 3000 });

    // Cancel
    await dialog.getByRole("button", { name: /cancel/i }).first().click();
    await expect(dialog).not.toBeVisible({ timeout: 3000 });

    // Node still on canvas
    await expect(
      page.locator(`.react-flow__node`).filter({ hasText: wsName }).first()
    ).toBeVisible({ timeout: 5000 });
  });
});
