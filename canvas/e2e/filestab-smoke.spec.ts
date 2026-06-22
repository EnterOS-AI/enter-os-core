import { test, expect } from "@playwright/test";

/**
 * Smoke test for the PR #10 FilesTab split. Exercises the UI end-to-end:
 * - creates a workspace on the platform
 * - opens the detail panel
 * - switches to the Files tab
 * - confirms tree, toolbar, and editor panels render (the three extracted
 *   sibling components: FileTree, FilesToolbar, FileEditor)
 * - saves a screenshot for visual review
 *
 * Requires platform on :8080 and canvas on :3000.
 */
test("FilesTab renders after split", async ({ page, request }) => {
  // Clean slate
  const { workspaces } = await request
    .get("http://localhost:8080/workspaces")
    .then(async (r) => ({ workspaces: (await r.json()) as Array<{ id: string; name: string }> }));
  for (const w of workspaces) {
    await request.delete(`http://localhost:8080/workspaces/${w.id}?confirm=true`, {
      headers: { "X-Confirm-Name": w.name },
    });
  }

  // Create a workspace
  const created = await request
    .post("http://localhost:8080/workspaces", {
      data: { name: "FilesTab Smoke", tier: 1, runtime: "langgraph" },
      headers: { "Content-Type": "application/json" },
    })
    .then((r) => r.json());
  const wsId = created.id as string;

  // Register so status flips online (so detail panel content loads cleanly)
  await request.post("http://localhost:8080/registry/register", {
    data: { id: wsId, url: "http://localhost:9999", agent_card: { name: "Smoke", skills: [] } },
    headers: { "Content-Type": "application/json" },
  });

  await page.goto("/");
  await expect(page).toHaveTitle(/Molecule AI/);

  // Screenshot: landing
  await page.screenshot({ path: "/tmp/filestab-1-landing.png", fullPage: false });

  // Dismiss any onboarding overlay if present (best-effort)
  const skip = page.getByText(/skip guide/i).first();
  if (await skip.isVisible().catch(() => false)) await skip.click();

  // Click the workspace node — title text is unique
  const node = page.getByText("FilesTab Smoke").first();
  await node.waitFor({ timeout: 10_000 });
  await node.click();

  // Side panel should open
  await page.waitForTimeout(300);
  await page.screenshot({ path: "/tmp/filestab-2-panel.png", fullPage: false });

  // Switch to Files tab. The tab bar overflows-x and buttons off-screen
  // resist the usual click path. Use Playwright's force-click on the
  // hidden button; this fires a real React onClick.
  // Tab button text is "⊞ Files" (icon + label). Use hasText substring.
  const filesBtn = page.locator("button").filter({ hasText: "Files" });
  await filesBtn.first().scrollIntoViewIfNeeded();
  await filesBtn.first().click({ force: true });

  await page.waitForTimeout(1200); // let files API load + render the 3 split components
  await page.screenshot({ path: "/tmp/filestab-3-files.png", fullPage: false });

  // Hard assertion: all three split components are visible.
  // FilesToolbar: "+ New", "Upload", "Export", "Clear" buttons.
  // FileTree: the config.yaml file from the Go provisioner's default template.
  // FileEditor: the empty-state placeholder "Select a file to edit".
  const toolbarNew = page.getByRole("button", { name: /new/i });
  const toolbarUpload = page.getByRole("button", { name: /upload/i });
  const treeFile = page.getByText("config.yaml");
  const editorEmpty = page.getByText(/select a file/i);

  await expect(toolbarNew.first()).toBeVisible({ timeout: 5_000 });
  await expect(toolbarUpload.first()).toBeVisible({ timeout: 5_000 });
  await expect(treeFile.first()).toBeVisible({ timeout: 5_000 });
  await expect(editorEmpty.first()).toBeVisible({ timeout: 5_000 });

  // Cleanup
  await request.delete(`http://localhost:8080/workspaces/${wsId}?confirm=true`, {
    headers: { "X-Confirm-Name": "FilesTab Smoke" },
  });
});
