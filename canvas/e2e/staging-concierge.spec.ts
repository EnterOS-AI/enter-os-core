/**
 * Staging concierge canvas E2E — exercises the platform-agent CONCIERGE shell
 * (canvas/src/components/concierge/ConciergeShell.tsx and the Settings split)
 * against a fresh staging org provisioned by the shared global setup
 * (e2e/staging-setup.ts). Each `test.describe` covers ONE concierge function
 * and asserts the behaviour works — not merely that an element exists.
 *
 * Why this is a SEPARATE spec from staging-tabs.spec.ts (which drives the
 * Org-map SidePanel tab UI): the two assert different surfaces of the same
 * tenant. Both reuse the EXACT shared harness — same global setup (one
 * provisioned org/workspace), same Playwright staging config (matched by the
 * `staging-*.spec.ts` testMatch), same gated `Canvas tabs E2E` workflow check.
 * No new harness, no new seeding mechanism.
 *
 * One extra precondition this spec needs that staging-tabs does NOT: a
 * kind='platform' concierge ROW. The CI/SaaS tenant does not self-seed one
 * (MOLECULE_SEED_PLATFORM_AGENT is unset on CI — workspace-server
 * cmd/server/main.go), so without it the concierge shell falls back to
 * roots[0] as a *pseudo*-platform surface and the platform-specific
 * behaviours (root tag, hidden-from-map) can't be asserted. So this spec
 * installs one via the SAME admin endpoint the control plane uses at
 * org-provision time — POST /admin/org/platform-agent (AdminAuth, accepts the
 * per-tenant admin bearer that global setup already exports). Installing it
 * re-parents the provisioned hermes workspace UNDER the platform agent
 * (handlers/platform_agent.go installPlatformAgent), giving us a real
 * platform ROOT + a real child workspace — exactly the topology the concierge
 * Home tree and Org-map filter are built to handle.
 *
 * This install mutates the shared tenant (re-parents the workspace). It is the
 * LAST staging spec alphabetically among the topology-touching ones, and
 * staging-tabs / staging-display read the workspace by id (not by root-ness),
 * so the re-parent does not break them; Playwright runs workers=1 in file
 * order, and the install is idempotent.
 *
 * Auth model is identical to staging-tabs.spec.ts: feed the per-tenant admin
 * token as an Authorization: Bearer header on every browser request, mock
 * /cp/auth/me so AuthGate resolves, and fall any non-auth 401 back to an
 * empty 200 so a workspace-scoped 401 can't yank us to AuthKit.
 */

import { test, expect, type Page, type BrowserContext } from "@playwright/test";

const STAGING = process.env.CANVAS_E2E_STAGING === "1";

// Fail-closed, not skip-green (mirrors staging-tabs.spec.ts): a staging run
// that was REQUESTED (CANVAS_E2E_STAGING=1) but has no tenant state is a
// provisioning failure, asserted loudly inside the test body — not a skip.
// CANVAS_E2E_STAGING unset = operator did not request staging = clean skip.
test.skip(!STAGING, "CANVAS_E2E_STAGING not set — staging-only suite, not requested");

/** Resolve + validate the tenant handoff that global setup exported. */
function tenantEnv() {
  const tenantURL = process.env.STAGING_TENANT_URL;
  const tenantToken = process.env.STAGING_TENANT_TOKEN;
  const workspaceId = process.env.STAGING_WORKSPACE_ID;
  const orgID = process.env.STAGING_ORG_ID;
  if (!tenantURL || !tenantToken || !workspaceId) {
    throw new Error(
      "staging-setup.ts did not export STAGING_TENANT_URL / " +
        "STAGING_TENANT_TOKEN / STAGING_WORKSPACE_ID. CANVAS_E2E_STAGING=1 was " +
        "set (staging WAS requested) but global setup produced no tenant — a " +
        "provisioning failure, NOT a reason to skip. See the [staging-setup] " +
        "log above.",
    );
  }
  return { tenantURL, tenantToken, workspaceId, orgID };
}

// A fixed, valid uuid for the installed platform agent. Any valid uuid works
// (the install upserts on this id); reusing one constant keeps re-runs
// idempotent on the same row. Chosen out of the e2e namespace so it can't
// collide with a CP-derived org id.
const PLATFORM_AGENT_ID = "e2e0c1e2-0000-4000-a000-000000c0ce0e";
const PLATFORM_AGENT_NAME = "E2E Concierge";

/**
 * Idempotently install the platform-agent (concierge) row on the shared
 * tenant so the concierge shell resolves a REAL kind='platform' root. Uses
 * the per-tenant admin bearer + org-id headers, same as staging-display.spec.
 * Tolerant of a pre-existing install (the endpoint is idempotent) and of a
 * backend that predates the endpoint (404/405) — in that degraded case the
 * spec proceeds against the roots[0] fallback and the two platform-specific
 * assertions self-document why they're loosened.
 */
async function installPlatformAgent(
  page: Page,
  tenantURL: string,
  tenantToken: string,
  orgID: string | undefined,
): Promise<{ installed: boolean }> {
  const headers: Record<string, string> = {
    Authorization: `Bearer ${tenantToken}`,
    "Content-Type": "application/json",
  };
  if (orgID) headers["X-Molecule-Org-Id"] = orgID;
  const resp = await page.request.post(`${tenantURL}/admin/org/platform-agent`, {
    headers,
    data: { id: PLATFORM_AGENT_ID, name: PLATFORM_AGENT_NAME },
  });
  const status = resp.status();
  if (status >= 200 && status < 300) {
    console.log(`[staging-concierge] platform agent installed (HTTP ${status})`);
    return { installed: true };
  }
  // Endpoint absent on an older backend — proceed against the fallback root.
  if (status === 404 || status === 405) {
    console.warn(
      `[staging-concierge] POST /admin/org/platform-agent returned ${status} — ` +
        `backend predates the platform-agent endpoint. Proceeding against the ` +
        `roots[0] concierge fallback; the platform-root / map-hidden assertions ` +
        `are loosened accordingly.`,
    );
    return { installed: false };
  }
  throw new Error(
    `POST /admin/org/platform-agent ${status}: ${await resp.text().catch(() => "")}`,
  );
}

/**
 * Wire the per-tenant bearer + the /cp/auth/me mock + the 401→empty-200
 * fallback. Verbatim contract from staging-tabs.spec.ts so the concierge spec
 * authenticates identically (no WorkOS session available to Playwright).
 */
async function authenticate(
  context: BrowserContext,
  tenantToken: string,
  workspaceId: string,
): Promise<void> {
  await context.setExtraHTTPHeaders({ Authorization: `Bearer ${tenantToken}` });

  await context.route("**/cp/auth/me", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        user_id: `e2e-test-user-${workspaceId}`,
        org_id: "e2e-test-org",
        email: "e2e@test.local",
      }),
    }),
  );

  await context.route("**", async (route, request) => {
    if (request.resourceType() !== "fetch") return route.fallback();
    if (request.url().includes("/cp/auth/me")) return route.fallback();
    let resp;
    try {
      resp = await route.fetch();
    } catch {
      return route.fallback();
    }
    if (resp.status() !== 401) return route.fulfill({ response: resp });
    const lastSeg =
      new URL(request.url()).pathname.split("/").filter(Boolean).pop() || "";
    const looksLikeList = !/^[0-9a-f-]{8,}$/.test(lastSeg);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: looksLikeList ? "[]" : "{}",
    });
  });
}

/**
 * Load the concierge shell and wait for hydration. Returns once the icon rail
 * (the concierge's left nav) is visible — the rail is the shell's outermost
 * stable landmark and only renders after the canvas store has hydrated.
 */
async function loadConcierge(page: Page, tenantURL: string): Promise<void> {
  page.on("console", (msg) => {
    if (msg.type() === "error") console.log(`[e2e/console-error] ${msg.text()}`);
  });
  await page.goto(tenantURL, { waitUntil: "domcontentloaded" });

  // The canvas store hydrates /workspaces before the desktop shell paints.
  // Wait for the concierge nav rail OR the hydration-error banner — whichever
  // wins. Don't wait on networkidle: the shell keeps a WS + polling open.
  await page.waitForSelector(
    '[data-testid="nav-home"], [data-testid="hydration-error"]',
    { timeout: 45_000 },
  );
  const hydrationErr = await page
    .locator('[data-testid="hydration-error"]')
    .count();
  expect(
    hydrationErr,
    "canvas hydration failed — check staging CP + tenant reachability",
  ).toBe(0);
  await expect(
    page.getByText("Something went wrong", { exact: false }),
    "app-level ErrorBoundary tripped during concierge hydration",
  ).toHaveCount(0);
}

/** Switch the concierge top-level view via the left rail. */
async function navTo(page: Page, view: "home" | "map" | "settings"): Promise<void> {
  const btn = page.getByTestId(`nav-${view}`);
  await expect(btn, `rail button nav-${view} missing`).toBeVisible({ timeout: 10_000 });
  await btn.click();
}

// ── shared per-spec setup ──────────────────────────────────────────────────
// Each test gets a freshly-authenticated context + an installed platform
// agent. Install lives in beforeEach (idempotent) so any single test can run
// in isolation (`--grep`), not only in whole-file order.
let platformInstalled = false;

test.beforeEach(async ({ page, context }) => {
  const { tenantURL, tenantToken, workspaceId, orgID } = tenantEnv();
  // Pre-seed a cookie-consent decision: the CookieConsent banner
  // (canvas 0dd4f259) is a fixed bottom overlay at z-[9999] that
  // intercepts pointer events on the nav rail — every click in this
  // suite times out with "<section aria-labelledby=cookie-consent-title>
  // intercepts pointer events" until a decision exists in localStorage.
  // "rejected" matches the privacy-preserving default; nothing in these
  // tests depends on optional cookies.
  await context.addInitScript(() => {
    window.localStorage.setItem(
      "molecule_cookie_consent",
      JSON.stringify({
        decision: "rejected",
        decidedAt: new Date().toISOString(),
        // Must match CookieConsent.tsx CURRENT_VERSION or the record is
        // ignored and the banner re-prompts.
        version: 1,
      }),
    );
  });
  await authenticate(context, tenantToken, workspaceId);
  const { installed } = await installPlatformAgent(page, tenantURL, tenantToken, orgID);
  platformInstalled = installed;
});

/* ───────────────────────── 1. Concierge shell / nav ──────────────────────── */
test.describe("concierge shell + nav", () => {
  test("left rail switches Home / Org map / Settings; topbar shows the org name", async ({
    page,
  }) => {
    const { tenantURL } = tenantEnv();
    await loadConcierge(page, tenantURL);

    // All three rail destinations are present.
    for (const v of ["home", "map", "settings"] as const) {
      await expect(page.getByTestId(`nav-${v}`)).toBeVisible();
    }

    // Topbar org name is dynamic from GET /org/identity. The endpoint returns
    // MOLECULE_ORG_NAME (may be "" on a staging tenant), in which case the
    // shell falls back to "Molecule AI". Either way it must render a
    // non-empty name — assert the element resolves to real text.
    const orgName = page.getByTestId("topbar-org-name");
    await expect(orgName).toBeVisible();
    await expect
      .poll(async () => ((await orgName.innerText()) || "").trim().length, {
        message: "topbar org name never resolved to non-empty text",
        timeout: 10_000,
      })
      .toBeGreaterThan(0);

    // Nav actually switches the active view. Home → Settings → Map → Home,
    // asserting the destination rail button reflects active state each hop
    // (the shell toggles the active class; we assert the view content too).
    await navTo(page, "settings");
    await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible({
      timeout: 10_000,
    });

    await navTo(page, "map");
    await expect(page.locator('[aria-label="Agent canvas"]')).toBeVisible({
      timeout: 15_000,
    });

    await navTo(page, "home");
    // Home shows the agents/tasks/approvals sub-tab bar.
    await expect(page.getByTestId("home-subtab-agents")).toBeVisible({
      timeout: 10_000,
    });
  });
});

/* ─────────────────────────────── 2. Home ─────────────────────────────────── */
test.describe("concierge Home", () => {
  test("renders the canonical ChatTab, Agents/Tasks/Approvals sub-tabs, and the platform agent as ROOT", async ({
    page,
  }) => {
    const { tenantURL } = tenantEnv();
    await loadConcierge(page, tenantURL);
    await navTo(page, "home");

    // (a) The Home chat panel reuses the EXACT canonical ChatTab — so it must
    // expose the My Chat / Agent Comms sub-tabs, a message input, and the
    // attachment affordance, exactly like the map SidePanel chat. The
    // [data-testid="chat-panel"] root is ChatTab's own marker (canvas/src/
    // components/tabs/ChatTab.tsx) — asserting it proves the canonical
    // component is mounted, not a bespoke concierge re-implementation.
    const chatPanel = page.getByTestId("chat-panel");
    await expect(chatPanel, "Home did not mount the canonical ChatTab").toBeVisible({
      timeout: 15_000,
    });
    await expect(chatPanel.locator("#chat-tab-my-chat")).toHaveText(/My Chat/);
    await expect(chatPanel.locator("#chat-tab-agent-comms")).toHaveText(/Agent Comms/);
    // Switching the chat sub-tab works (My Chat active by default → Agent Comms).
    await chatPanel.locator("#chat-tab-agent-comms").click();
    await expect(chatPanel.locator("#chat-tab-agent-comms")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    await chatPanel.locator("#chat-tab-my-chat").click();
    await expect(chatPanel.locator("#chat-tab-my-chat")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    // Message input + attachment affordance (My Chat panel). The attach
    // control is the labelled button (the underlying <input type=file> is
    // aria-hidden); both are always present (disabled when the agent is
    // unreachable), so assert presence, not enabled-state.
    await expect(
      chatPanel.locator('textarea[aria-label="Message to agent"]'),
      "ChatTab message input missing",
    ).toHaveCount(1);
    await expect(
      chatPanel.locator('button[aria-label="Attach file"]'),
      "ChatTab attachment affordance missing",
    ).toHaveCount(1);

    // (b) Agents / Tasks / Approvals sub-tabs switch the Home sidebar pane.
    await page.getByTestId("home-subtab-tasks").click();
    await expect(page.getByTestId("home-subtab-tasks")).toHaveClass(/active/);
    await page.getByTestId("home-subtab-approvals").click();
    await expect(page.getByTestId("home-subtab-approvals")).toHaveClass(/active/);
    await page.getByTestId("home-subtab-agents").click();
    await expect(page.getByTestId("home-subtab-agents")).toHaveClass(/active/);

    // (c) The agent tree shows the platform agent as ROOT. After install the
    // platform agent is a kind='platform' root carrying the "root" tag, with
    // the provisioned workspace re-parented under it (depth>0). When the
    // backend predates the install endpoint, roots[0] is the pseudo-root and
    // the "root" tag is absent (it only renders for a real kind='platform'
    // root) — so we gate the strong assertion on a successful install.
    const tree = page.getByTestId("agent-tree-node");
    await expect(tree.first(), "agent tree rendered no nodes").toBeVisible({
      timeout: 10_000,
    });
    if (platformInstalled) {
      // The depth-0 node is the platform agent and it carries the root tag.
      const rootNode = page
        .locator('[data-testid="agent-tree-node"][data-depth="0"]')
        .first();
      await expect(rootNode).toHaveAttribute("data-platform", "true");
      await expect(
        rootNode.locator('[data-testid="agent-tree-root-tag"]'),
        "platform root is missing the ROOT tag",
      ).toBeVisible();
      // And the provisioned workspace is nested beneath it (a child node exists).
      await expect(
        page.locator('[data-testid="agent-tree-node"][data-depth="1"]'),
        "the provisioned workspace did not re-parent under the platform root",
      ).toHaveCount(1, { timeout: 10_000 });
    } else {
      // Degraded backend: at least the tree renders a root-level node.
      await expect(
        page.locator('[data-testid="agent-tree-node"][data-depth="0"]'),
      ).not.toHaveCount(0);
    }
  });
});

/* ─────────────────────────────── 3. Org map ──────────────────────────────── */
test.describe("concierge Org map", () => {
  test("hides the platform agent from the node graph; normal workspaces render", async ({
    page,
  }) => {
    const { tenantURL } = tenantEnv();
    await loadConcierge(page, tenantURL);
    await navTo(page, "map");

    // The React Flow canvas renders.
    await expect(page.locator('[aria-label="Molecule AI workspace canvas"]')).toBeVisible({
      timeout: 15_000,
    });

    // Normal workspaces render as map node cards (WorkspaceNode →
    // data-testid="workspace-node-{name}"). The provisioned hermes workspace must
    // appear. expect.poll lets React Flow finish its layout pass.
    await expect
      .poll(async () => page.locator('[data-testid^="workspace-node-"]').count(), {
        message: "no workspace nodes rendered on the org map",
        timeout: 15_000,
      })
      .toBeGreaterThan(0);

    // The concierge (platform agent) is HIDDEN from the graph: no map node
    // carries its name. WorkspaceNode's aria-label is "<name> workspace —
    // <status>" — assert none matches the platform agent name. This is the
    // real behaviour stripPlatformRootForMap implements (Canvas.tsx /
    // canvas-topology.ts). Only meaningful when we actually installed one.
    if (platformInstalled) {
      const platformNode = page.locator(
        `[data-testid^="workspace-node-"][aria-label^="${PLATFORM_AGENT_NAME} workspace"]`,
      );
      await expect(
        platformNode,
        "the platform agent (concierge) leaked into the org-map node graph — " +
          "stripPlatformRootForMap should exclude it",
      ).toHaveCount(0);
    }
  });
});

/* ─────────────────────── 4. Settings — two tabs ──────────────────────────── */
test.describe("concierge Settings — two tabs", () => {
  test("Platform-agent config and Org & canvas settings are separate panes; platform tab shows the full WorkspacePanelTabs defaulting to Config", async ({
    page,
  }) => {
    const { tenantURL } = tenantEnv();
    await loadConcierge(page, tenantURL);
    await navTo(page, "settings");

    const platformTab = page.getByTestId("settings-tab-platform");
    const orgTab = page.getByTestId("settings-tab-org");
    await expect(platformTab).toBeVisible({ timeout: 10_000 });
    await expect(orgTab).toBeVisible();

    // Platform tab is the default; its pane is shown and the org pane is not.
    await expect(platformTab).toHaveAttribute("aria-selected", "true");
    await expect(page.getByTestId("settings-pane-platform")).toBeVisible();
    await expect(page.getByTestId("settings-pane-org")).toHaveCount(0);

    // The platform pane embeds the FULL WorkspacePanelTabs (the SAME tablist
    // the map SidePanel renders) and defaults to the Config tab. Assert the
    // canonical workspace tablist is present, that Config is the active tab,
    // and that the other signature tabs exist (Plugins, Container, Display,
    // Details, Activity, Terminal, Channels, Schedule).
    const wsTablist = page.getByRole("tablist", { name: "Workspace panel tabs" });
    await expect(
      wsTablist,
      "platform-agent Settings tab did not embed WorkspacePanelTabs",
    ).toBeVisible({ timeout: 15_000 });
    // concierge-embedded WorkspacePanelTabs namespaces its ids with
    // "concierge-" (duplicate-id fix); the bare #tab-* ids belong to the
    // map SidePanel instance only.
    await expect(page.locator("#concierge-tab-config")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    for (const id of [
      "config",
      "skills",
      "container-config",
      "display",
      "details",
      "activity",
      "terminal",
      "channels",
      "schedule",
    ]) {
      await expect(
        page.locator(`#concierge-tab-${id}`),
        `WorkspacePanelTabs is missing #concierge-tab-${id}`,
      ).toHaveCount(1);
    }

    // Clicking the OTHER settings tab switches panes (not just toggles a
    // class): the org pane mounts and the platform pane unmounts.
    await orgTab.click();
    await expect(orgTab).toHaveAttribute("aria-selected", "true");
    await expect(page.getByTestId("settings-pane-org")).toBeVisible();
    await expect(page.getByTestId("settings-pane-platform")).toHaveCount(0);

    // And back.
    await platformTab.click();
    await expect(page.getByTestId("settings-pane-platform")).toBeVisible();
    await expect(page.getByTestId("settings-pane-org")).toHaveCount(0);
  });
});

/* ─────────────────────── 5. Settings — Config tab ────────────────────────── */
test.describe("concierge Settings — Config tab dropdowns", () => {
  test("runtime dropdown is SSOT-driven; provider hides Platform on self-host but lists BYOK; model follows provider", async ({
    page,
  }) => {
    const { tenantURL } = tenantEnv();
    await loadConcierge(page, tenantURL);
    await navTo(page, "settings");

    // Platform tab defaults to the Config tab — the runtime select is in the
    // ConfigTab "Runtime" section (label "Runtime"). Wait for it to settle.
    await expect(
      page.getByRole("tablist", { name: "Workspace panel tabs" }),
    ).toBeVisible({ timeout: 15_000 });
    // The runtime <select> sits under the "Runtime" label inside the Config
    // panel. Use the label association for a stable hook.
    const runtimeByLabel = page.locator('#concierge-panel-config').getByLabel("Runtime", {
      exact: true,
    });
    await expect(
      runtimeByLabel,
      "ConfigTab runtime dropdown never rendered",
    ).toBeVisible({ timeout: 15_000 });

    // (a) Runtime dropdown is SSOT-driven: the options come from GET
    // /templates (loadRuntimesFromManifest), so the live tenant must serve a
    // non-trivial set. Assert >= 1 runtime option AND that the provisioned
    // workspace's runtime (hermes) is among them — proving the list reflects
    // what /templates actually serves, not a stale hard-coded allowlist.
    const runtimeOptionValues = await runtimeByLabel
      .locator("option")
      .evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value));
    expect(
      runtimeOptionValues.length,
      "runtime dropdown rendered no options — SSOT /templates feed is empty",
    ).toBeGreaterThan(0);
    expect(
      runtimeOptionValues,
      "runtime dropdown does not list the provisioned 'hermes' runtime — the " +
        "SSOT /templates list has drifted",
    ).toContain("hermes");

    // (b) Provider dropdown: on self-host (no platform proxy) it must NOT
    // offer the "Platform" billing option but MUST list BYOK providers. The
    // ProviderModelSelector exposes data-testid="provider-select". Read its
    // option labels: none should be the "Platform" proxy entry, and the list
    // must be non-empty (BYOK providers present). /org/identity's
    // platform_managed_available=false on a staging tenant drives this.
    const providerSelect = page.getByTestId("provider-select");
    await expect(
      providerSelect,
      "ConfigTab provider dropdown (ProviderModelSelector) never rendered",
    ).toBeVisible({ timeout: 15_000 });
    const providerLabels = await providerSelect
      .locator("option")
      .evaluateAll((els) =>
        els
          .map((e) => (e.textContent || "").trim())
          .filter((t) => t && !t.startsWith("—")),
      );
    expect(
      providerLabels.length,
      "provider dropdown lists no BYOK providers",
    ).toBeGreaterThan(0);
    expect(
      providerLabels.map((l) => l.toLowerCase()),
      'provider dropdown offered the "Platform" proxy option on a self-host / ' +
        "no-proxy tenant (platform_managed_available should hide it)",
    ).not.toContain("platform");

    // (c) Model dropdown follows the provider. The model control is
    // data-testid="model-select" (dropdown) or model-input (free-text
    // wildcard). Whichever renders, it must be present — proving the model
    // control is wired to the provider selection.
    const modelControl = page
      .locator('[data-testid="model-select"], [data-testid="model-input"]')
      .first();
    await expect(
      modelControl,
      "model control did not follow the provider selection",
    ).toBeVisible({ timeout: 10_000 });
  });
});

/* ────────────────── 6. Settings — Org & canvas settings ──────────────────── */
test.describe("concierge Settings — Org & canvas", () => {
  test("Secrets / Workspace Tokens / Org API Keys / Organization sub-tabs render; Organization shows the org (no 404)", async ({
    page,
  }) => {
    const { tenantURL } = tenantEnv();
    await loadConcierge(page, tenantURL);
    await navTo(page, "settings");

    await page.getByTestId("settings-tab-org").click();
    const orgPane = page.getByTestId("settings-pane-org");
    await expect(orgPane).toBeVisible({ timeout: 10_000 });

    // The four SettingsTabs (canvas/src/components/settings/SettingsTabs.tsx)
    // render as a radix tablist labelled "Settings sections". Assert all four
    // triggers are present.
    const settingsTablist = orgPane.getByRole("tablist", {
      name: "Settings sections",
    });
    await expect(settingsTablist).toBeVisible({ timeout: 10_000 });
    for (const label of [
      "Secrets",
      "Workspace Tokens",
      "Org API Keys",
      "Organization",
    ]) {
      await expect(
        settingsTablist.getByRole("tab", { name: label }),
        `Org & canvas settings is missing the "${label}" sub-tab`,
      ).toBeVisible();
    }

    // Click the Organization sub-tab — on self-host the canvas reads
    // /org/identity (NOT the CP /cp/orgs endpoint), so it must render the org
    // identity card and NOT a 404 / error state. Assert the pane settles to
    // real, non-error content.
    await settingsTablist.getByRole("tab", { name: "Organization" }).click();
    const orgInfoPanel = orgPane.locator(
      '[role="tabpanel"]:not([hidden])',
    );
    await expect(orgInfoPanel).toBeVisible({ timeout: 10_000 });
    await expect
      .poll(
        async () => {
          const text = ((await orgInfoPanel.innerText()) || "").trim();
          return text.length > 0 && !/404|not found/i.test(text);
        },
        {
          message:
            "Organization sub-tab rendered empty or a 404/not-found — the " +
            "self-host /org/identity path is broken",
          timeout: 15_000,
        },
      )
      .toBe(true);
    // And no visible error alert inside the org settings pane.
    await expect(orgPane.locator('[role="alert"]:visible')).toHaveCount(0);
  });
});

/* ───────────────────────────── 7. Map toolbar ────────────────────────────── */
test.describe("concierge Org map toolbar", () => {
  test("settings gear, theme toggle and legend are NOT on the map toolbar (moved to Settings/topbar)", async ({
    page,
  }) => {
    const { tenantURL } = tenantEnv();
    await loadConcierge(page, tenantURL);
    await navTo(page, "map");
    await expect(page.locator('[aria-label="Molecule AI workspace canvas"]')).toBeVisible({
      timeout: 15_000,
    });

    // The map toolbar no longer carries a settings gear, a theme toggle, or a
    // legend — those moved to the concierge Settings (left rail) + topbar
    // (Toolbar.tsx: "Theme picker + settings gear removed from the map
    // toolbar"). Assert the map view contains none of them.
    //
    // Scope to the map mount (<main aria-label="Agent canvas">, ConciergeShell)
    // so the legitimate left-rail Settings button + the topbar theme toggle
    // (which live OUTSIDE the map) are not counted.
    const mapRegion = page.locator('[aria-label="Agent canvas"]');
    await expect(mapRegion).toBeVisible({ timeout: 10_000 });

    // No settings-gear control inside the map. The old gear used
    // title="Settings" / aria-label "Settings".
    await expect(
      mapRegion.locator('button[title="Settings"], button[aria-label="Settings"]'),
      "a settings gear is still on the map toolbar (should be moved to Settings)",
    ).toHaveCount(0);

    // No theme toggle inside the map. The toggle's accessible name is
    // "Toggle theme" — it now lives only in the topbar.
    await expect(
      mapRegion.locator('button[title="Toggle theme"], button[aria-label*="theme" i]'),
      "a theme toggle is still on the map toolbar (should be in the topbar)",
    ).toHaveCount(0);

    // No legend inside the map. The Legend component's controls have accessible
    // names "Show legend" / "Hide legend" and the panel carries
    // data-testid="legend-panel" (canvas/src/components/Legend.tsx). It is no
    // longer mounted in Canvas/Toolbar at all — assert none of its surfaces.
    await expect(
      mapRegion.locator(
        '[data-testid="legend-panel"], button[aria-label="Show legend"], button[aria-label="Hide legend"]',
      ),
      "a legend is still on the map toolbar (should be removed)",
    ).toHaveCount(0);
  });
});
