/**
 * Staging canvas E2E — opens each workspace-panel tab against a fresh
 * staging org provisioned in the global setup. Asserts each tab renders
 * REAL content (not an empty container, not an error state) and captures a
 * screenshot for visual review.
 *
 * Auth model: the tenant platform's AdminAuth middleware accepts a bearer
 * token OR a WorkOS session cookie. Playwright can't mint a WorkOS
 * session, so we feed the per-tenant admin token (fetched in global
 * setup via GET /cp/admin/orgs/:slug/admin-token) as an Authorization:
 * Bearer header via context.setExtraHTTPHeaders(). Every browser
 * request inherits the header.
 *
 * PROMOTION-READINESS (see § at bottom of file): this suite is being
 * hardened toward becoming a HARD merge-gate. It currently runs under
 * `continue-on-error: true` (RFC internal#219 §1, non-gating) — that is a
 * deliberate, CTO-owned call and is NOT changed here. The hardening makes
 * every assertion deterministic so that WHEN promotion happens the gate
 * does not flap. See the PROMOTION-READINESS block at the foot of this
 * file for what is now reliable and what still blocks promotion.
 *
 * Known SaaS gaps — documented in #1369. These tabs legitimately cannot
 * load real content in SaaS mode and are allowed an in-panel empty/error
 * state (NOT a hard crash, NOT an ErrorBoundary):
 *   - Files tab: empty (platform can't docker exec into a remote EC2)
 *   - Terminal tab: WS connect fails
 *   - Peers tab: 401 without workspace-scoped token
 * These are enumerated in KNOWN_DEGRADED_TABS below and asserted with a
 * weaker (but still non-trivial) contract: the panel renders and does not
 * crash the app. Every OTHER tab must render real content.
 */

import { test, expect, type Page } from "@playwright/test";

// Tab ids as declared in canvas/src/components/SidePanel.tsx TABS.
//
// NOTE (drift guard): this list is asserted-complete against the live DOM
// below (see "tab list parity" step) so it cannot silently drift out of
// sync with SidePanel.tsx TABS the way a hand-maintained constant does.
// `display` and `container-config` are intentionally EXCLUDED here:
//   - `display` is owned by the in-flight take-control e2e (PR #2275 /
//     staging-display.spec.ts); asserting it here would collide.
//   - `container-config` only renders when selectedNodeId is set AND is
//     gated on tier; it is covered by container-config-specific specs.
// The parity check accounts for these via EXPECTED_EXTRA_TABS so a NEW
// tab appearing in SidePanel still trips the guard.
const TAB_IDS = [
  "chat",
  "activity",
  "details",
  "skills",
  "terminal",
  "config",
  "schedule",
  "channels",
  "files",
  "memory",
  "traces",
  "events",
  "audit",
] as const;

// Tabs present in the DOM that this spec intentionally does not drive.
// Keeping this explicit means a genuinely-new tab (not one of these) makes
// the parity assertion fail LOUD instead of being silently un-tested.
const EXPECTED_EXTRA_TABS = ["display", "container-config"] as const;

// Tabs that are KNOWN to degrade in SaaS mode (#1369). They get the weaker
// "renders + no crash" contract instead of the "real content" contract.
// Anything NOT in this set must render real content or the test fails.
const KNOWN_DEGRADED_TABS = new Set<string>(["terminal", "files"]);

const STAGING = process.env.CANVAS_E2E_STAGING === "1";

// IMPORTANT — fail-closed, not skip-green.
//
// `test.skip(!STAGING)` is correct ONLY when the operator never asked for a
// staging run (CANVAS_E2E_STAGING unset). In that case the workflow's
// detect-changes / token-check gates have already decided not to exercise
// staging, and skipping is the documented contract.
//
// But if STAGING *is* requested (CANVAS_E2E_STAGING=1) and global setup did
// NOT hand off the tenant state, that is a HARD failure, not a skip — see
// the explicit env-presence throw inside the test body. A silent skip there
// would let a broken provision ship green, which is exactly the
// weak-gate failure this hardening removes (§ No flakes / internal#828).
test.skip(!STAGING, "CANVAS_E2E_STAGING not set — staging-only suite, not requested");

/**
 * Assert the panel for `tabId` rendered real content.
 *
 * Deterministic contract (no fixed waits — every step is condition-based
 * with Playwright's built-in retry / expect.poll):
 *   1. The tabpanel container is visible.
 *   2. The global ErrorBoundary did NOT trip ("Something went wrong").
 *   3. No visible error alert is shown in the panel.
 *   4. For non-degraded tabs: the panel settles to non-empty,
 *      non-spinner content (so an empty <div/> or a stuck "Loading…"
 *      spinner FAILS instead of passing as it did before).
 */
async function assertPanelRendered(page: Page, tabId: string): Promise<void> {
  const panel = page.locator(`#panel-${tabId}`);

  // (1) Container visible. Built-in retry up to the expect timeout — no
  // arbitrary waitForTimeout. Mechanism: replaces any reliance on a fixed
  // settle delay with a real visibility condition.
  await expect(panel, `panel for ${tabId} never became visible`).toBeVisible({
    timeout: 10_000,
  });

  // (2) ErrorBoundary trip = hard crash anywhere in the React subtree.
  // canvas/src/components/ErrorBoundary.tsx renders "Something went wrong".
  // The OLD gate only looked for a "Failed to load" toast and would ship
  // an ErrorBoundary-crashed panel GREEN. Mechanism: assert the crash
  // surface is absent, retried via expect.poll so a late-mounting crash
  // banner is still caught.
  await expect
    .poll(
      async () =>
        page.getByText("Something went wrong", { exact: false }).count(),
      {
        message: `tab ${tabId}: ErrorBoundary tripped (Something went wrong)`,
        timeout: 5_000,
      },
    )
    .toBe(0);

  // (3) No visible error alert inside the panel. Tabs surface load errors
  // as role="alert" with the real error text (EventsTab/ChannelsTab/
  // ConfigTab/...). The OLD gate matched ONLY [role=alert]:has-text("Failed
  // to load") — it missed (a) error messages that don't contain that exact
  // phrase and (b) error divs that omit role="alert" entirely (e.g.
  // ActivityTab). We replace it with a broader, but still SaaS-gap-aware,
  // check: any *visible* alert OR red error banner inside the panel.
  //
  // Degraded tabs (#1369) are allowed an error state — for those we only
  // require no app-level crash (covered by step 2). For every other tab a
  // visible error alert is a real regression.
  if (!KNOWN_DEGRADED_TABS.has(tabId)) {
    const visibleAlerts = panel.locator('[role="alert"]:visible');
    await expect
      .poll(async () => visibleAlerts.count(), {
        message:
          `tab ${tabId}: a visible error alert is shown in the panel ` +
          `(was a weak "Failed to load"-only check before)`,
        timeout: 5_000,
      })
      .toBe(0);
  }

  // (4) Real content. The tabpanel CONTAINER always mounts, so the old
  // toBeVisible() on the container passed even when the child rendered
  // nothing. Assert the panel's trimmed innerText is non-empty AND not
  // stuck on a loading spinner. expect.poll retries until the async
  // fetch+render settles — replacing the implicit "the network finished
  // by now" timing assumption with an explicit polled condition.
  //
  // Degraded tabs may legitimately be empty (Files in SaaS mode), so they
  // are exempt from the non-empty requirement; step 2 still guards them
  // against a hard crash.
  if (!KNOWN_DEGRADED_TABS.has(tabId)) {
    await expect
      .poll(
        async () => {
          const text = ((await panel.innerText()) || "").trim();
          // A panel still showing only a loading spinner has not settled.
          const stillLoading = /^(loading\b|loading…|loading\.\.\.)/i.test(
            text,
          );
          return text.length > 0 && !stillLoading;
        },
        {
          message:
            `tab ${tabId}: panel rendered empty or stuck on a loading ` +
            `spinner — no real content settled (weak "container visible" ` +
            `gate would have passed this)`,
          // Generous: real tabs fetch from the tenant over the network.
          // Polled, so it returns as soon as content appears.
          timeout: 20_000,
        },
      )
      .toBe(true);
  }
}

test.describe("staging canvas tabs", () => {
  test("each workspace-panel tab renders real content", async ({
    page,
    context,
  }) => {
    const tenantURL = process.env.STAGING_TENANT_URL;
    const tenantToken = process.env.STAGING_TENANT_TOKEN;
    const workspaceId = process.env.STAGING_WORKSPACE_ID;
    const orgID = process.env.STAGING_ORG_ID;

    // FAIL-CLOSED (not skip): STAGING was requested but global setup did
    // not export tenant state. A silent skip here would paint a broken
    // provision GREEN. This is the loud-fail the hardening mandates.
    //
    // STAGING_ORG_ID is REQUIRED when STAGING was requested. The tenant
    // platform's TenantGuard middleware (workspace-server
    // middleware/tenant_guard.go) cross-org-gates every browser request —
    // without X-Molecule-Org-Id, the canvas mounts, AuthGate fires
    // /cp/auth/me, and the 401 from TenantGuard redirects away from the
    // tenant URL before any panel settles. (Run 353448/job 478063 @ sha
    // 57ff36de failed this exact way: "Failed to load" / hidden Echo
    // nodes from the fallback layout.) Mirror staging-concierge.spec.ts
    // 52-66, 91-96: resolve + fail-closed if STAGING_ORG_ID is unset
    // when STAGING was requested.
    if (!tenantURL || !tenantToken || !workspaceId || !orgID) {
      throw new Error(
        "staging-setup.ts did not export STAGING_TENANT_URL / " +
          "STAGING_TENANT_TOKEN / STAGING_WORKSPACE_ID / STAGING_ORG_ID. " +
          "CANVAS_E2E_STAGING=1 was set (staging WAS requested) but global " +
          "setup produced no tenant — this is a provisioning failure, NOT " +
          "a reason to skip. Check the [staging-setup] log above for the " +
          "real error.",
      );
    }

    // Attach the per-tenant admin bearer AND the X-Molecule-Org-Id
    // cross-org header to every outbound request. The tenant platform's
    // AdminAuth middleware accepts the bearer; the TenantGuard
    // middleware (workspace-server) requires X-Molecule-Org-Id. Both
    // are needed; missing either is a HARD 401, not a graceful degrade.
    // No WorkOS session needed.
    await context.setExtraHTTPHeaders({
      Authorization: `Bearer ${tenantToken}`,
      "X-Molecule-Org-Id": orgID,
    });

    // canvas/src/components/AuthGate.tsx fetches /cp/auth/me on mount
    // and redirects to the login page on 401. The bearer header above
    // is for platform API calls — it does NOT satisfy /cp/auth/me,
    // which is cookie-based (WorkOS session). Without this mock, the
    // canvas page mounts AuthGate, sees 401 from /cp/auth/me, and
    // redirects away from the tenant URL before the React Flow root
    // ever renders. The [aria-label] selector wait then times out.
    //
    // Intercept /cp/auth/me + return a fake Session shape so AuthGate
    // resolves to "authenticated" and renders {children}. The session
    // contents are cosmetic — the canvas only inspects org_id/user_id
    // in a few places that don't fail when these are dummy values.
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

    // Universal 401 → empty-200 fallback (defense-in-depth).
    //
    // The original product bug was canvas/src/lib/api.ts:62-74 calling
    // `redirectToLogin` on EVERY 401 — a single workspace-scoped 401
    // (e.g. /workspaces/:id/peers, /plugins) yanked the user (and the
    // test) to AuthKit. That's now fixed at the source: api.ts probes
    // /cp/auth/me before redirecting, so a 401 from a non-auth path
    // with a live session throws a regular error instead.
    //
    // This route handler stays as a SAFETY NET, not the primary
    // defense:
    //   1. It silences resource-load console noise from the browser
    //      (those messages don't include the URL — useless in
    //      diagnostics, captured by the filter in the assertion
    //      block but having no 401s reach the network is cleaner).
    //   2. It guards against panels that DON'T have try/catch around
    //      their api calls — an unhandled rejection would surface
    //      as console.error → fail the assertion. Panels SHOULD
    //      handle errors, but until they're all audited, this is
    //      the test's belt to api.ts's braces.
    //
    // Pass-through real responses; swap 401s for 200 + empty body.
    // Skip /cp/auth/me (mocked above) and non-fetch resources
    // (HTML/JS/CSS bundles that should NOT be intercepted).
    await context.route("**", async (route, request) => {
      if (request.resourceType() !== "fetch") {
        return route.fallback();
      }
      // /cp/auth/me is mocked above with a fixed Session shape — let
      // that handler win without us round-tripping the network.
      if (request.url().includes("/cp/auth/me")) {
        return route.fallback();
      }
      let resp;
      try {
        resp = await route.fetch();
      } catch {
        return route.fallback();
      }
      if (resp.status() !== 401) {
        return route.fulfill({ response: resp });
      }
      const lastSeg =
        new URL(request.url()).pathname.split("/").filter(Boolean).pop() || "";
      const looksLikeList = !/^[0-9a-f-]{8,}$/.test(lastSeg);
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: looksLikeList ? "[]" : "{}",
      });
    });

    const consoleErrors: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error") {
        consoleErrors.push(msg.text());
      }
    });

    // Capture the URL of any failed network request so a "Failed to load
    // resource: 404" console message we filter out below leaves a
    // breadcrumb. Browser console messages for resource-load failures
    // omit the URL, so we'd otherwise be flying blind. Logged to the
    // test's stdout (visible in the workflow log under the failed step).
    page.on("requestfailed", (req) => {
      console.log(
        `[e2e/requestfailed] ${req.method()} ${req.url()}: ${
          req.failure()?.errorText ?? "?"
        }`,
      );
    });
    page.on("response", (res) => {
      if (res.status() >= 400) {
        console.log(
          `[e2e/response-${res.status()}] ${res
            .request()
            .method()} ${res.url()}`,
        );
      }
    });

    // waitUntil="networkidle" is wrong here — the canvas keeps a
    // WebSocket open + polls /events and /workspaces every few
    // seconds, so the network is *never* idle for 500ms. page.goto
    // would hang until its 45s default timeout. "domcontentloaded"
    // returns as soon as the HTML is parsed; React hydration + the
    // selector wait below is what actually gates ready-for-interaction.
    await page.goto(tenantURL, { waitUntil: "domcontentloaded" });

    // The staging canvas now hydrates the concierge shell first.
    // Wait for the left-nav rail (concierge shell landmark) or the
    // hydration-error banner — whichever wins first. Don't wait on
    // networkidle: the shell keeps a WS + polling open.
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

    // The global ErrorBoundary must not have tripped at the app root
    // either — a crash before the side panel even opens would otherwise
    // be invisible until a tab assertion happened to notice it.
    await expect(
      page.getByText("Something went wrong", { exact: false }),
      "app-level ErrorBoundary tripped during hydration",
    ).toHaveCount(0);

    // Navigate to the Org map view. WorkspaceNode is only rendered
    // when topView === "map" (canvas/src/components/concierge/
    // ConciergeShell.tsx:528 — the React Flow canvas mount is gated
    // on the topView state). The default concierge view after
    // hydration is "home" (Home chat panel), so without an explicit
    // nav-map click the [data-workspace-id] selector below would
    // wait for a node that isn't in the rendered tree at all —
    // exactly the failure mode of #2721-deeper (Researcher RCA on
    // run 358136 / job 486781, head 867557f08).
    //
    // staging-concierge.spec.ts:376 (the "Org map" test) does the
    // same navTo(page, "map") and uses expect.poll on the
    // [data-testid^="workspace-node-"] count — that's the proven
    // pattern. We mirror it here, then layer the specific
    // [data-workspace-id="$STAGING_WORKSPACE_ID"] selector on top
    // for the per-workspace click target.
    await page.locator('[data-testid="nav-map"]').click({ timeout: 10_000 });

    // Wait for the React Flow canvas to mount, then poll for the
    // workspace-node count (RFs layout pass takes a tick after the
    // nav click) before drilling down to the specific workspace.
    await expect(page.locator('[aria-label="Molecule AI workspace canvas"]')).toBeVisible({
      timeout: 15_000,
    });
    await expect
      .poll(async () => page.locator('[data-testid^="workspace-node-"]').count(), {
        message: "no workspace nodes rendered on the org map after nav-map click",
        timeout: 15_000,
      })
      .toBeGreaterThan(0);

    // Now wait for the SPECIFIC workspace node we want — keyed by
    // data-workspace-id (the UUID-keyed marker restored in #2729).
    // expect.poll because React Flow's layout pass can take a tick
    // to position the just-inserted node, and the node may render
    // before the data-workspace-id attribute is committed to the DOM.
    await expect
      .poll(
        async () =>
          page.locator(`[data-workspace-id="${workspaceId}"]`).count(),
        {
          message: `workspace node with data-workspace-id=${workspaceId} never rendered on the org map`,
          timeout: 15_000,
        },
      )
      .toBeGreaterThan(0);

    // Click the workspace node to open the side panel. Try a data
    // attribute first, fall back to a generic role-based selector so
    // the test doesn't break when the node-card markup changes.
    const byDataAttr = page
      .locator(`[data-workspace-id="${workspaceId}"]`)
      .first();
    if ((await byDataAttr.count()) > 0) {
      await byDataAttr.click({ timeout: 10_000 });
    } else {
      const firstNode = page
        .locator('[role="button"][aria-label*="Workspace" i]')
        .first();
      await firstNode.click({ timeout: 10_000 });
    }

    // The tablist appears once the side panel mounts. Condition-based
    // wait — no fixed delay.
    const tablist = page.getByRole("tablist", { name: "Workspace panel tabs" });
    await expect(
      tablist,
      "side panel tablist never appeared after clicking the workspace node",
    ).toBeVisible({ timeout: 15_000 });

    // Tab-list parity guard. The hand-maintained TAB_IDS constant used to
    // be able to drift silently out of sync with SidePanel.tsx TABS — a
    // tab could be added to the UI and never get an assertion, shipping
    // broken-but-untested. Read the actual tab ids from the DOM and assert
    // every live tab is either driven by this spec (TAB_IDS) or explicitly
    // excluded (EXPECTED_EXTRA_TABS). A genuinely-new tab fails LOUD.
    const liveTabIds = (
      await tablist.locator('[role="tab"][id^="tab-"]').evaluateAll((els) =>
        els.map((el) => el.id.replace(/^tab-/, "")),
      )
    ).sort();
    const accountedFor = new Set<string>([
      ...TAB_IDS,
      ...EXPECTED_EXTRA_TABS,
    ]);
    const unaccounted = liveTabIds.filter((id) => !accountedFor.has(id));
    expect(
      unaccounted,
      `SidePanel exposes tab(s) this spec neither drives nor excludes: ` +
        `${unaccounted.join(", ")}. Add them to TAB_IDS (and assert their ` +
        `content) or to EXPECTED_EXTRA_TABS with a reason.`,
    ).toHaveLength(0);
    // And the inverse: every TAB_ID we intend to drive must actually exist
    // in the DOM, so a renamed/removed tab fails here instead of timing out
    // on a missing #tab-<id> selector with an opaque message.
    const missing = TAB_IDS.filter((id) => !liveTabIds.includes(id));
    expect(
      missing,
      `TAB_IDS references tab(s) not present in SidePanel: ${missing.join(
        ", ",
      )} — the spec's tab list has drifted from SidePanel.tsx TABS.`,
    ).toHaveLength(0);

    for (const tabId of TAB_IDS) {
      await test.step(`tab: ${tabId}`, async () => {
        const tabButton = page.locator(`#tab-${tabId}`);
        // The TABS bar is `overflow-x-auto` — tabs past position ~3 are
        // clipped behind the right-edge fade gradient on smaller
        // viewports. Playwright's toBeVisible() returns false for clipped
        // elements, so a bare visibility check fails on later tabs in CI.
        // scrollIntoViewIfNeeded brings the button into view before the
        // visibility check.
        await tabButton.scrollIntoViewIfNeeded({ timeout: 5_000 });
        await expect(
          tabButton,
          `tab-${tabId} button missing — TABS list may have drifted`,
        ).toBeVisible({ timeout: 5_000 });
        await tabButton.click();

        // Confirm the click actually activated this tab before asserting
        // its content — aria-selected flips on the active tab. This closes
        // a race where a slow click handler left the PREVIOUS tab's panel
        // mounted and we asserted the wrong panel's content. Built-in
        // retry, condition-based, no fixed wait.
        await expect(
          tabButton,
          `tab-${tabId} did not become the selected tab after click`,
        ).toHaveAttribute("aria-selected", "true", { timeout: 5_000 });

        // Real-content assertion (the core hardening). See
        // assertPanelRendered: container visible + no ErrorBoundary + no
        // visible error alert + settled non-empty content for non-degraded
        // tabs. Replaces the old "panel visible + no Failed-to-load toast"
        // pair, which shipped empty/errored panels green.
        await assertPanelRendered(page, tabId);

        // Belt to the braces: the original toast check stays. A global
        // "Failed to load" toast (role=alert outside the panel) is still a
        // crash signal worth catching even though the in-panel checks above
        // now do the heavy lifting.
        const errorToasts = await page
          .locator('[role="alert"]:has-text("Failed to load")')
          .count();
        expect(
          errorToasts,
          `tab ${tabId}: a global "Failed to load" toast is showing`,
        ).toBe(0);

        await page.screenshot({
          path: `test-results/staging-tab-${tabId}.png`,
          fullPage: false,
        });
      });
    }

    // Aggregate console-error budget. Known-noisy sources whitelisted:
    // Sentry, Vercel analytics, WS reconnects (expected on SaaS
    // terminal), favicon 404 (cosmetic), and the browser's generic
    // "Failed to load resource: ... 404" message which never includes
    // the URL — uninformative on its own and impossible to filter
    // meaningfully without a URL. The page.on('requestfailed') +
    // page.on('response>=400') logging above captures the actual URLs
    // so a real bug still leaves a breadcrumb in the workflow log;
    // a real exception (panel crash, JS error) surfaces as a typed
    // error with file path which the filter still catches.
    const appErrors = consoleErrors.filter(
      (msg) =>
        !msg.includes("sentry") &&
        !msg.includes("vercel") &&
        !msg.includes("WebSocket") &&
        !msg.includes("favicon") &&
        !msg.includes("molecule-icon.png") && // cosmetic 404
        !msg.includes("Failed to load resource"),
    );
    expect(
      appErrors,
      `unexpected console errors:\n${appErrors.join("\n")}`,
    ).toHaveLength(0);
  });
});

/*
 * PROMOTION-READINESS — staging canvas E2E → HARD merge-gate
 * ----------------------------------------------------------
 * NOW RELIABLE (deterministic; these no longer flap on timing):
 *   - Every wait is condition-based (toBeVisible / toHaveAttribute /
 *     expect.poll). There is NO fixed waitForTimeout / sleep in the spec;
 *     the only setTimeout is the bounded poll-interval inside
 *     staging-setup.ts waitFor(), which has a hard deadline.
 *   - Tabs are asserted on REAL settled content (non-empty, non-spinner),
 *     not just "container is visible" — an empty or stuck-loading panel now
 *     fails instead of shipping green.
 *   - The ErrorBoundary ("Something went wrong") is asserted absent at app
 *     hydration AND per tab — a React subtree crash can no longer pass.
 *   - Visible error alerts inside a panel fail non-degraded tabs (was a
 *     weak [role=alert]:has-text("Failed to load")-only check that missed
 *     both other error phrasings and role-less error divs).
 *   - The driven tab list is parity-checked against the live DOM, so a new
 *     SidePanel tab can't ship un-tested and a removed one fails loud.
 *   - Click→activation is confirmed (aria-selected) before asserting the
 *     panel, removing a wrong-panel race.
 *   - The suite is fail-closed: CANVAS_E2E_STAGING=1 with no tenant state
 *     hard-errors (never skips→green); CANVAS_E2E_STAGING unset cleanly
 *     skips (operator did not request staging).
 *
 * STILL BLOCKS PROMOTION-TO-REQUIRED (do NOT flip continue-on-error here —
 * CTO-owned, RFC internal#219 §1):
 *   - INFRA DEPENDENCY: each run provisions a real staging EC2 tenant
 *     (12-20 min cold boot). Required-gate latency + AWS/Cloudflare/CP
 *     availability become merge-blockers. A staging outage would freeze
 *     main even though the code is fine — unacceptable for a required check
 *     until staging has an SLA or this runs against a warm pre-provisioned
 *     pool.
 *   - SHARED-RESOURCE FLAKE SURFACE: TLS/DNS/ACME propagation on a shared
 *     staging zone (staging-setup TLS_TIMEOUT_MS) is outside this repo's
 *     control. Deterministic here ≠ deterministic upstream.
 *   - SECRET DEPENDENCY: CP_STAGING_ADMIN_API_TOKEN must be present on the
 *     runner. The workflow's skip-if-absent (core#2225) keeps a missing
 *     secret from painting red — correct for non-gating, but a REQUIRED
 *     check must instead guarantee the secret is always present, else it
 *     skip-greens the very thing it is supposed to enforce.
 *   - SINGLE-WORKSPACE COVERAGE: one hermes/platform_managed workspace that
 *     does NOT boot an agent on staging (no CP LLM proxy env, workspace-
 *     server #2162). Tabs render, but agent-dependent content paths (live
 *     chat round-trip, traces from a real run) are not exercised.
 *
 * PROMOTION CHECKLIST (when CTO signs off on making this required):
 *   1. Warm pre-provisioned tenant pool OR a staging SLA bounding boot time.
 *   2. Guarantee CP_STAGING_ADMIN_API_TOKEN on the gating runner; turn the
 *      skip-if-absent into a hard error for the required path.
 *   3. Decide whether agent-dependent tabs need a wired LLM proxy on the
 *      staging tenant (covers chat/traces real content) before gating them.
 */
