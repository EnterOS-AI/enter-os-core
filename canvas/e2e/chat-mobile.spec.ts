import { test, expect } from "@playwright/test";
import { startEchoRuntime } from "./fixtures/echo-runtime";
import { seedWorkspace, startHeartbeat, cleanupWorkspace, seedChatHistory } from "./fixtures/chat-seed";


test.describe("MobileChat", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    // Seed chat history so the "chat panel loads" test starts from a
    // non-empty transcript state. Without this, the panel may render the
    // empty placeholder on first open, making the hasHistory assertion
    // false-red even though the fixture is otherwise correct.
    await seedChatHistory(workspaceId, [
      { role: "user", content: "Hello from mobile seed" },
      { role: "agent", content: "Echo: Hello from mobile seed" },
    ]);

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
    // Wait for the actual readiness signal: the chat-panel must be visible
    // inside the mobile shell (not just in the DOM). This replaces the
    // previous fixed 10s wait that raced app hydration.
    await page.waitForSelector("[data-testid='chat-panel']:visible", { timeout: 15_000 });
    // Wait for the workspace status to flip to online and the textarea to be enabled.
    await expect(page.locator("textarea").first()).toBeEnabled({ timeout: 15_000 });
    // Dismiss onboarding guide if present.
    const skipGuide = page.getByText("Skip guide");
    if (await skipGuide.isVisible().catch(() => false)) {
      await skipGuide.click();
    }
  });

  test("chat panel loads without error", async ({ page }) => {
    const chat = page.locator("[data-testid='chat-panel']");
    const emptyState = chat.getByText("Send a message to start chatting.");
    // The workspace is seeded with chat history; empty-state here is a
    // hydration/render failure, not a valid initial condition. Assert the
    // seeded transcript renders AND the empty placeholder is absent.
    await expect(chat.getByText("Hello from mobile seed", { exact: true })).toBeVisible({ timeout: 5_000 });
    await expect(chat.getByText("Echo: Hello from mobile seed")).toBeVisible({ timeout: 5_000 });
    await expect(emptyState).toBeHidden();
  });

  test("send text message and receive echo response", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Mobile test message");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Mobile test message", { exact: true })).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText("Echo: Mobile test message")).toBeVisible({ timeout: 15_000 });
  });

  test("history persists across reload", async ({ page }) => {
    const textarea = page.locator("textarea").first();
    await textarea.fill("Mobile persistence");
    await page.getByRole("button", { name: /Send/ }).first().click();

    await expect(page.getByText("Echo: Mobile persistence")).toBeVisible({ timeout: 15_000 });

    // Reload and deterministically wait for the chat-history GET that
    // rehydrates the transcript to come back 2xx, rather than racing a
    // fixed-timeout render assertion against an in-flight fetch. The
    // server now persists the a2a_receive row SYNCHRONOUSLY before the
    // send's 200 (workspace-server logA2ASuccess), so the row is
    // guaranteed present by the time this GET runs — the wait is for
    // hydration latency, not for a still-racing write.
    const historyResponse = page.waitForResponse(
      (resp) =>
        resp.url().includes("/chat-history") &&
        resp.request().method() === "GET" &&
        resp.status() === 200,
      { timeout: 15_000 },
    );
    await page.reload();
    await page.waitForSelector("[data-testid='chat-panel']:visible", { timeout: 15_000 });
    await historyResponse;

    await expect(page.getByText("Mobile persistence", { exact: true })).toBeVisible();
    await expect(page.getByText("Echo: Mobile persistence")).toBeVisible();
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
