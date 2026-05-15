import { test, expect } from "@playwright/test";
import { startEchoRuntime } from "./fixtures/echo-runtime";
import { seedWorkspace, startHeartbeat, cleanupWorkspace } from "./fixtures/chat-seed";


test.describe("MobileChat", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    cleanup = async () => {
      stopHeartbeat();
      await echo.stop();
    };
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    // Navigate directly to the mobile chat view.
    await page.goto(`/?m=chat&a=${workspaceId}`);
    await page.waitForSelector("[data-testid='chat-panel']", { timeout: 10_000 });
    // Wait for the workspace status to flip to online and the textarea to be enabled.
    await expect(page.locator("textarea").first()).toBeEnabled({ timeout: 15_000 });
    // Dismiss onboarding guide if present.
    const skipGuide = page.getByText("Skip guide");
    if (await skipGuide.isVisible().catch(() => false)) {
      await skipGuide.click();
    }
  });

  test("chat panel loads without error", async ({ page }) => {
    const hasEmptyState = await page.getByText("Send a message to start chatting.").isVisible().catch(() => false);
    const hasHistory = await page.locator("[data-testid='chat-panel']").locator("div").count() > 3;
    expect(hasEmptyState || hasHistory).toBeTruthy();
  });

  test("send text message and receive echo response", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Mobile test message");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Mobile test message")).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText("Echo: Mobile test message")).toBeVisible({ timeout: 15_000 });
  });

  test("history persists across reload", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Mobile persistence");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Echo: Mobile persistence")).toBeVisible({ timeout: 15_000 });

    await page.reload();
    await page.waitForSelector("[data-testid='chat-panel']", { timeout: 10_000 });

    await expect(page.getByText("Mobile persistence", { exact: true })).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText("Echo: Mobile persistence")).toBeVisible({ timeout: 5_000 });
  });

  test("composer auto-grows with multi-line text", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    const initialHeight = await textarea.evaluate((el: HTMLElement) => el.offsetHeight);

    await textarea.fill("Line 1\nLine 2\nLine 3\nLine 4\nLine 5");
    await page.waitForTimeout(300);

    const grownHeight = await textarea.evaluate((el: HTMLElement) => el.offsetHeight);
    expect(grownHeight).toBeGreaterThan(initialHeight);
  });

  test("file attachment in mobile chat", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Mobile file test");

    const fileInput = page.locator("[data-testid='chat-panel'] input[type='file']").first();
    await fileInput.setInputFiles({
      name: "mobile.txt",
      mimeType: "text/plain",
      buffer: Buffer.from("mobile secret"),
    });

    await expect(page.getByText("mobile.txt")).toBeVisible({ timeout: 3_000 });

    await page.getByRole("button", { name: /Send/ }).first().click();
    await expect(page.getByText("Echo: Mobile file test")).toBeVisible({ timeout: 15_000 });
  });
});
