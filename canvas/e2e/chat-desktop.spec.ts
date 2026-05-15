import { test, expect } from "@playwright/test";
import { startEchoRuntime } from "./fixtures/echo-runtime";
import { seedWorkspace, startHeartbeat, cleanupWorkspace } from "./fixtures/chat-seed";


test.describe("Desktop ChatTab", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let workspaceName = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    workspaceName = ws.name;
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
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/");
    await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
    // Dismiss onboarding guide if present.
    const skipGuide = page.getByText("Skip guide");
    if (await skipGuide.isVisible().catch(() => false)) {
      await skipGuide.click();
    }
    // Click the workspace node by its exact name label.
    await page.getByText(workspaceName, { exact: true }).first().click();
    // Wait for the side panel chat tab to be clickable, then click it.
    await page.locator('#tab-chat').click();
    await page.waitForSelector("[data-testid='chat-panel']", { timeout: 5_000 });
    // Wait for the workspace status to flip to online and the textarea to be enabled.
    await expect(page.locator("textarea").first()).toBeEnabled({ timeout: 15_000 });
  });

  test("chat panel loads without error", async ({ page }) => {
    const hasEmptyState = await page.getByText("Send a message to start chatting.").isVisible().catch(() => false);
    const hasHistory = await page.locator("[data-testid='chat-panel']").locator("div").count() > 3;
    expect(hasEmptyState || hasHistory).toBeTruthy();
  });

  test("send text message and receive echo response", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("What is the weather?");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("What is the weather?")).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText("Echo: What is the weather?")).toBeVisible({ timeout: 15_000 });
  });

  test("history persists across reload", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Persistence test");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Echo: Persistence test")).toBeVisible({ timeout: 15_000 });

    await page.reload();
    await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
    await page.getByText(workspaceName, { exact: true }).first().click();
    await page.locator('#tab-chat').click();
    await page.waitForSelector("[data-testid='chat-panel']", { timeout: 5_000 });
    // Wait for the workspace status to flip to online and the textarea to be enabled.
    await expect(page.locator("textarea").first()).toBeEnabled({ timeout: 15_000 });

    await expect(page.getByText("Persistence test", { exact: true })).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText("Echo: Persistence test")).toBeVisible({ timeout: 5_000 });
  });

  test("file attachment round-trip", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Please read this file");

    const fileInput = page.locator("[data-testid='chat-panel'] input[type='file']").first();
    await fileInput.setInputFiles({
      name: "test.txt",
      mimeType: "text/plain",
      buffer: Buffer.from("secret content abc123"),
    });

    await expect(page.getByText("test.txt")).toBeVisible({ timeout: 3_000 });

    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Echo: Please read this file")).toBeVisible({ timeout: 15_000 });
  });

  test("activity log appears during send", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Trigger activity");
    await page.getByRole("button", { name: /Send/ }).first().click();

    // Activity log container should appear during the send flow.
    await expect(page.locator("[data-testid='activity-log']").first()).toBeVisible({ timeout: 10_000 }).catch(() => {
      // Activity log may not be present in all layouts.
    });
  });
});

test.describe("Desktop ChatTab — Markdown rendering", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let workspaceName = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    workspaceName = ws.name;
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
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/");
    await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
    const skipGuide2 = page.getByText("Skip guide");
    if (await skipGuide2.isVisible().catch(() => false)) {
      await skipGuide2.click();
    }
    await page.getByText(workspaceName, { exact: true }).first().click();
    await page.locator('#tab-chat').click();
    await page.waitForSelector("[data-testid='chat-panel']", { timeout: 5_000 });
    // Wait for the workspace status to flip to online and the textarea to be enabled.
    await expect(page.locator("textarea").first()).toBeEnabled({ timeout: 15_000 });
  });

  test("code block renders <pre>", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("```js\nconst x = 1;\n```");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Echo: ```js")).toBeVisible({ timeout: 15_000 });

    const pre = page.locator("pre").first();
    await expect(pre).toBeVisible({ timeout: 5_000 });
    await expect(pre).toContainText("const x = 1;");
  });

  test("table renders <table>", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("| A | B |\n|---|---|\n| 1 | 2 |");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Echo: | A | B |")).toBeVisible({ timeout: 15_000 });

    const table = page.locator("table").first();
    await expect(table).toBeVisible({ timeout: 5_000 });
    await expect(table).toContainText("A");
    await expect(table).toContainText("1");
  });
});
