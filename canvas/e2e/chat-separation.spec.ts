import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { startEchoRuntime } from "./fixtures/echo-runtime";
import {
  seedWorkspace,
  startHeartbeat,
  cleanupWorkspace,
  seedChatHistory,
  queryPsql,
} from "./fixtures/chat-seed";

const PLATFORM_URL = process.env.E2E_PLATFORM_URL ?? "http://localhost:8080";
const API = process.env.E2E_API_URL ?? PLATFORM_URL;

/** Enter the Org-map view so the Canvas (React Flow graph) mounts. */
async function enterMapView(page: Page): Promise<void> {
  const btn = page.getByTestId("nav-map");
  await expect(btn, "rail button nav-map missing").toBeVisible({ timeout: 10_000 });
  await btn.click();
}

/** Open the seeded workspace's Chat side panel (scoped to the visible panel). */
async function openChatPanel(page: Page, workspaceName: string): Promise<void> {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await enterMapView(page);
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });

  // Dismiss onboarding guide if present.
  const skipGuide = page.getByText("Skip guide");
  if (await skipGuide.isVisible().catch(() => false)) {
    await skipGuide.click();
  }

  // Scope to the map-side panel (#2587) so we don't accidentally hit the
  // hidden ConciergeShell copy of ChatTab.
  await page.getByTestId(`workspace-node-${workspaceName}`).click();
  await page.locator("#tab-chat").click();
  await page.waitForSelector("#panel-chat [data-testid='chat-panel']:visible", {
    timeout: 5_000,
  });
  await expect(page.locator("#panel-chat [data-testid='chat-panel']:visible textarea").first()).toBeEnabled({
    timeout: 15_000,
  });
}

const panelLocator = (page: Page) =>
  page.locator("#panel-chat [data-testid='chat-panel']:visible");
/** Post a message to the workspace via the A2A proxy so activity rows exist.
 *  `source` determines the auth shape, which in turn determines
 *  activity_logs.source_id:
 *    - "canvas": no workspace-resolving auth, no X-Workspace-ID → callerID
 *      empty → source_id NULL (the /activity?source=canvas filter).
 *    - "agent": workspace bearer token → callerID = workspace →
 *      source_id = workspace_id (the /activity?source=agent filter).
 */
async function postA2AMessage(
  workspaceId: string,
  source: "canvas" | "agent",
  text: string,
  workspaceAuthToken: string,
) {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (source === "agent") {
    headers.Authorization = `Bearer ${workspaceAuthToken}`;
  }
  // canvas-source intentionally sends no bearer and no X-Workspace-ID so the
  // proxy cannot derive a workspace callerID. This is the only way to produce
  // the source_id NULL rows that the source=canvas endpoint keys on.

  const res = await fetch(`${PLATFORM_URL}/workspaces/${workspaceId}/a2a`, {
    method: "POST",
    headers,
    body: JSON.stringify({
      method: "message/send",
      params: {
        message: {
          role: "user",
          parts: [{ kind: "text", text }],
        },
      },
    }),
  });
  if (!res.ok) {
    throw new Error(`A2A post failed: ${res.status} ${await res.text()}`);
  }
}

/** Extract the text payload from an activity_logs request_body envelope. */
function requestBodyText(reqBody: unknown): string {
  if (typeof reqBody !== "object" || reqBody === null) return "";
  const params = (reqBody as Record<string, unknown>).params;
  if (typeof params !== "object" || params === null) return "";
  const message = (params as Record<string, unknown>).message;
  if (typeof message !== "object" || message === null) return "";
  const parts = (message as Record<string, unknown>).parts;
  if (!Array.isArray(parts)) return "";
  for (const part of parts) {
    if (typeof part === "object" && part !== null && typeof (part as Record<string, unknown>).text === "string") {
      return (part as Record<string, string>).text;
    }
  }
  return "";
}

test.describe("Chat Sub-Tabs", () => {
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
    await openChatPanel(page, workspaceName);
  });

  test("chat tab shows My Chat and Agent Comms sub-tabs", async ({ page }) => {
    const panel = panelLocator(page);
    await expect(panel.getByRole("tab", { name: "My Chat" })).toBeVisible();
    await expect(panel.getByRole("tab", { name: "Agent Comms" })).toBeVisible();
  });

  test("My Chat is selected by default", async ({ page }) => {
    const myChatBtn = panelLocator(page).getByRole("tab", { name: "My Chat" });
    await expect(myChatBtn).toHaveAttribute("aria-selected", "true");
  });

  test("switching to Agent Comms shows different content", async ({ page }) => {
    const panel = panelLocator(page);
    await panel.getByRole("tab", { name: "Agent Comms" }).click();

    // Agent Comms should be selected and My Chat's textarea should not be visible.
    await expect(
      panel.getByRole("tab", { name: "Agent Comms" }),
    ).toHaveAttribute("aria-selected", "true");
    await expect(panel.locator("textarea").first()).not.toBeVisible();
  });

  test("My Chat has input box, Agent Comms does not", async ({ page }) => {
    const panel = panelLocator(page);

    // My Chat has the textarea.
    await expect(panel.locator("textarea").first()).toBeVisible();

    // Switch to Agent Comms.
    await panel.getByRole("tab", { name: "Agent Comms" }).click();
    await expect(panel.locator("textarea").first()).not.toBeVisible();
  });

  test("switching back to My Chat preserves messages", async ({ page }) => {
    const panel = panelLocator(page);

    // Send a message so there is content to preserve.
    const textarea = panel.locator("textarea").first();
    await textarea.fill("Persistence check");
    await page.getByRole("button", { name: /Send/ }).first().click();
    await expect(
      panel.getByText("Echo: Persistence check"),
    ).toBeVisible({ timeout: 15_000 });

    // Switch to Agent Comms and back.
    await panel.getByRole("tab", { name: "Agent Comms" }).click();
    await panel.getByRole("tab", { name: "My Chat" }).click();

    // Message should still be there.
    await expect(panel.getByText("Persistence check", { exact: true })).toBeVisible();
    await expect(panel.getByText("Echo: Persistence check")).toBeVisible();
  });
});

test.describe("Activity API Source Filter", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let authToken = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    authToken = ws.authToken;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    // Seed BOTH source classes deterministically through the real A2A proxy:
    //  - canvas-source: no workspace-resolving auth → callerID empty →
    //    source_id NULL (matches /activity?source=canvas).
    //  - agent-source: workspace bearer token → callerID = workspace →
    //    source_id = workspace_id (matches /activity?source=agent).
    await postA2AMessage(workspaceId, "canvas", "canvas source probe", authToken);
    await postA2AMessage(workspaceId, "agent", "agent source probe", authToken);

    cleanup = async () => {
      stopHeartbeat();
      await echo.stop();
    };
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test("source=canvas returns only canvas-initiated entries", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?source=canvas`,
      { headers: { Authorization: `Bearer ${authToken}` } },
    );
    expect(res.ok()).toBeTruthy();
    const entries = (await res.json()) as Array<{
      source_id: unknown;
      request_body: unknown;
    }>;
    expect(Array.isArray(entries)).toBeTruthy();
    // False-green guard: an empty array would make the loop below pass vacuously.
    expect(entries.length).toBeGreaterThan(0);
    for (const e of entries) {
      expect(e.source_id).toBeNull();
    }
    // The seeded canvas probe must be present; if source separation broke and
    // the canvas probe was logged as agent-sourced, this would fail.
    expect(entries.some((e) => requestBodyText(e.request_body) === "canvas source probe")).toBe(true);
  });

  test("source=agent returns only agent-initiated entries", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?source=agent`,
      { headers: { Authorization: `Bearer ${authToken}` } },
    );
    expect(res.ok()).toBeTruthy();
    const entries = (await res.json()) as Array<{
      source_id: unknown;
      request_body: unknown;
    }>;
    expect(Array.isArray(entries)).toBeTruthy();
    // False-green guard: an empty array would make the loop below pass vacuously.
    expect(entries.length).toBeGreaterThan(0);
    for (const e of entries) {
      expect(e.source_id).not.toBeNull();
    }
    // The seeded agent probe must be present; if source separation broke and
    // the agent probe was logged as canvas-sourced, this would fail.
    expect(entries.some((e) => requestBodyText(e.request_body) === "agent source probe")).toBe(true);
  });

  test("source=invalid returns 400", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?source=bogus`,
      { headers: { Authorization: `Bearer ${authToken}` } },
    );
    expect(res.status()).toBe(400);
  });

  test("source+type filters combine correctly (canvas)", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?type=a2a_receive&source=canvas`,
      { headers: { Authorization: `Bearer ${authToken}` } },
    );
    expect(res.ok()).toBeTruthy();
    const entries = (await res.json()) as Array<{
      activity_type: string;
      source_id: unknown;
      request_body: unknown;
    }>;
    expect(Array.isArray(entries)).toBeTruthy();
    // False-green guard: an empty array would make the loop below pass vacuously.
    expect(entries.length).toBeGreaterThan(0);
    for (const e of entries) {
      expect(e.activity_type).toBe("a2a_receive");
      expect(e.source_id).toBeNull();
    }
    expect(entries.some((e) => requestBodyText(e.request_body) === "canvas source probe")).toBe(true);
  });

  test("source+type filters combine correctly (agent)", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?type=a2a_receive&source=agent`,
      { headers: { Authorization: `Bearer ${authToken}` } },
    );
    expect(res.ok()).toBeTruthy();
    const entries = (await res.json()) as Array<{
      activity_type: string;
      source_id: unknown;
      request_body: unknown;
    }>;
    expect(Array.isArray(entries)).toBeTruthy();
    // False-green guard: an empty array would make the loop below pass vacuously.
    expect(entries.length).toBeGreaterThan(0);
    for (const e of entries) {
      expect(e.activity_type).toBe("a2a_receive");
      expect(e.source_id).not.toBeNull();
    }
    expect(entries.some((e) => requestBodyText(e.request_body) === "agent source probe")).toBe(true);
  });
});

test.describe("Data Flow — Initial Prompt in Chat", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let workspaceName = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    workspaceName = ws.name;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    // Pre-seed chat history so the My Chat panel shows deterministic content.
    // Include double quotes to regression-test shell-safe JSON quoting in
    // seedChatHistory (CR2 #11517).
    await seedChatHistory(workspaceId, [
      { role: "user", content: 'Hello from seed with "quotes"' },
      { role: "agent", content: 'Hello back from seed with "quotes"' },
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
    await openChatPanel(page, workspaceName);
  });

  test("seeded chat history appears in My Chat", async ({ page }) => {
    const panel = panelLocator(page);
    await expect(panel.getByText('Hello from seed with "quotes"')).toBeVisible({ timeout: 5_000 });
    await expect(panel.getByText('Hello back from seed with "quotes"')).toBeVisible({ timeout: 5_000 });
  });

  test("My Chat empty state is not shown when history exists", async ({ page }) => {
    const panel = panelLocator(page);
    await expect(panel.getByText("No messages yet")).not.toBeVisible();
  });
});

const describeWithDb = process.env.E2E_DATABASE_URL
  ? test.describe
  : test.describe.skip;

describeWithDb("Chat seed DB round-trip", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    // Seed tricky payloads: double quotes, backslashes, apostrophes, and a
    // newline. If the JSON is mangled by shell/SQL quoting, the round-trip
    // assertion below will fail instead of silently passing.
    await seedChatHistory(workspaceId, [
      {
        role: "user",
        content: 'User said "hello" and \\backslash\\ plus an apostrophe\'s test',
      },
      {
        role: "agent",
        content: 'Agent replied "ok"\nwith a newline',
      },
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

  test("seeded jsonb round-trips exactly through psql", async () => {
    interface SeededActivityRow {
      id: string;
      workspace_id: string;
      activity_type: string;
      source_id: string | null;
      method: string;
      request_body: unknown;
      response_body: unknown;
      status: string;
      duration_ms: number;
      created_at: string;
    }

    const rows = queryPsql<
      SeededActivityRow[]
    >(`SELECT jsonb_agg(row_to_json(t) ORDER BY t.created_at) FROM (SELECT id, workspace_id, activity_type, source_id, method, request_body, response_body, status, duration_ms, created_at FROM activity_logs WHERE workspace_id = '${workspaceId}' ORDER BY created_at) t`)[0];

    expect(rows).toHaveLength(2);

    const [userRow, agentRow] = rows;

    expect(userRow.activity_type).toBe("a2a_receive");
    expect(userRow.source_id).toBeNull();
    expect(userRow.method).toBe("message/send");
    expect(userRow.request_body).toEqual({
      params: {
        message: {
          parts: [
            {
              kind: "text",
              text: 'User said "hello" and \\backslash\\ plus an apostrophe\'s test',
            },
          ],
        },
      },
    });
    expect(userRow.response_body).toEqual({});

    expect(agentRow.activity_type).toBe("a2a_receive");
    expect(agentRow.source_id).toBeNull();
    expect(agentRow.method).toBe("message/send");
    expect(agentRow.request_body).toEqual({});
    expect(agentRow.response_body).toEqual({
      result: {
        parts: [
          {
            kind: "text",
            text: 'Agent replied "ok"\nwith a newline',
          },
        ],
      },
    });
  });
});

test.describe("No JS Errors", () => {
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

  test("page loads without errors with chat sub-tabs", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (err) => errors.push(err.message));

    await openChatPanel(page, workspaceName);

    // Switch between tabs.
    const panel = panelLocator(page);
    await panel.getByRole("tab", { name: "Agent Comms" }).click();
    await panel.getByRole("tab", { name: "My Chat" }).click();

    const critical = errors.filter(
      (e) => !e.includes("WebSocket") && !e.includes("favicon") && !e.includes("hydration"),
    );
    expect(critical).toEqual([]);
  });
});
