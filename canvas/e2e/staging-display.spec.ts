/**
 * Staging canvas E2E — REAL desktop take-control path (core#2261 "Gap 1").
 *
 * This is the live-e2e gate that the existing staging-tabs.spec.ts does NOT
 * provide. staging-tabs only opens the 13 declared workspace-panel tabs
 * (TAB_IDS at staging-tabs.spec.ts:24-38 — `display` is NOT among them) and
 * asserts they render without a "Failed to load" toast. It never acquires
 * display control, never opens the noVNC WebSocket, and never asserts a
 * framebuffer frame arrives. The companion unit test
 * canvas/src/components/tabs/__tests__/DisplayTab.test.tsx mocks the RFB
 * constructor (vi.mock("@novnc/novnc"), see its lines 8/20-39) so NO real
 * WebSocket is ever opened there either. Result: a broken take-control path
 * (acquire → noVNC WS upgrade → ws-proxy → EIC → websockify → x11vnc → Xvfb)
 * ships GREEN. This spec closes that gap by exercising the REAL wire path
 * end to end against a live, desktop-capable staging workspace.
 *
 * What it asserts (the real path, no mocks):
 *   1. POST /workspaces/<id>/display/control/acquire returns 200 with a
 *      session_url that carries the signed token in its `#token=` fragment
 *      (mirrors workspace_display_control.go:signedDisplaySessionURL).
 *   2. Opening the noVNC WebSocket at session_url with the subprotocols
 *      ["binary", "molecule-display-token.<token>"] (exactly what the canvas
 *      sends — DisplayTab.tsx:339) UPGRADES (onopen fires, readyState===OPEN,
 *      no immediate 1006 abnormal close). A 1006 / 403 means the handshake
 *      failed somewhere in the proxy chain.
 *   3. At least one BINARY framebuffer message arrives on that socket — a
 *      real frame off x11vnc, not just a panel mount. RFB sends a
 *      ProtocolVersion banner ("RFB 003.00x\n") as the first server message,
 *      which proves the upstream VNC server is live behind the EIC tunnel.
 *
 * Auth model (important): the WS upgrade is gated by workspace-server
 * middleware.AdminAuth. A browser WebSocket CANNOT set an Authorization
 * header, so in production the canvas WS upgrade passes AdminAuth via the
 * same-origin-canvas path (wsauth_middleware.go:isSameOriginCanvas, which
 * keys off the Origin header the browser sets automatically on a same-origin
 * WS upgrade). We therefore open the socket from inside the browser page via
 * page.evaluate AFTER navigating to the tenant origin — so the browser sends
 * `Origin: https://<slug>.staging.moleculesai.app`, exactly as production
 * does. The acquire POST (which CAN carry a header) uses the per-tenant admin
 * bearer set on the context. This is the faithful production handshake, not a
 * synthetic one.
 *
 * Gate / cost: this test only runs when STAGING_DISPLAY_WORKSPACE_ID points
 * at a STANDING desktop-capable workspace (compute.display.mode ==
 * "desktop-control"). We deliberately do NOT provision one in the shared
 * staging-setup.ts: a desktop AMI boots in ~12-15 min and would tax the
 * existing tabs harness on every run. Standing that workspace up is a cost
 * item for the CTO (one always-on desktop EC2 on staging). Until that exists,
 * the test SKIPS loud. When the env IS present, any failure in
 * provision/acquire/upgrade is a HARD error — fail-closed, never silently
 * green (no "flaky" disposition: a 1006 names a broken proxy hop).
 */

import { test, expect } from "@playwright/test";

const STAGING = process.env.CANVAS_E2E_STAGING === "1";

// The standing desktop-capable workspace id. Absent => skip loud. This is
// the single knob that activates the gate; see file header for the cost note.
const DISPLAY_WS_ID = process.env.STAGING_DISPLAY_WORKSPACE_ID;

test.skip(!STAGING, "CANVAS_E2E_STAGING not set — skipping staging-only tests");
test.skip(
  !DISPLAY_WS_ID,
  "STAGING_DISPLAY_WORKSPACE_ID not set — no standing desktop-capable staging " +
    "workspace to exercise the take-control path. Set it to a workspace whose " +
    "compute.display.mode == 'desktop-control' to activate this real-e2e gate. " +
    "(Standing that workspace up is a CTO cost item — one always-on desktop EC2.)",
);

// How long we wait for the WS to upgrade + deliver the first frame. The EIC
// tunnel + websockify handshake adds real latency on top of the edge; budget
// generously but bounded, so a genuinely-dead path fails LOUD instead of
// hanging to the suite timeout.
const WS_UPGRADE_TIMEOUT_MS = 30_000;
const FIRST_FRAME_TIMEOUT_MS = 30_000;

test.describe("staging desktop take-control (real noVNC path)", () => {
  test("acquire → WS upgrades → first framebuffer frame arrives", async ({
    page,
    context,
  }) => {
    // The standing desktop workspace lives in its OWN standing org (it can't
    // live in the per-run ephemeral org — that gets torn down each run). When
    // STAGING_DISPLAY_SLUG is configured, staging-setup.ts resolves that org's
    // tenant URL / admin token / org id and exports them under STAGING_DISPLAY_*.
    // Fall back to the ephemeral org's exports only if the display org wasn't
    // separately configured (e.g. the desktop workspace happens to live in the
    // run's own tenant — not the expected topology, but supported).
    const tenantURL =
      process.env.STAGING_DISPLAY_TENANT_URL || process.env.STAGING_TENANT_URL;
    const tenantToken =
      process.env.STAGING_DISPLAY_TENANT_TOKEN || process.env.STAGING_TENANT_TOKEN;
    const orgID =
      process.env.STAGING_DISPLAY_ORG_ID || process.env.STAGING_ORG_ID;

    // Fail-closed: when the gate env IS present (we got past the skips above),
    // the rest of the staging context MUST be wired or this is a hard error,
    // never a silent pass. Mirrors staging-tabs.spec.ts:53-57.
    if (!tenantURL || !tenantToken) {
      throw new Error(
        "STAGING_DISPLAY_WORKSPACE_ID is set but no tenant URL/token is available " +
          "for the take-control gate. Set STAGING_DISPLAY_SLUG so staging-setup.ts " +
          "resolves STAGING_DISPLAY_TENANT_URL / STAGING_DISPLAY_TENANT_TOKEN for the " +
          "standing desktop org (or ensure the ephemeral STAGING_TENANT_* exports exist).",
      );
    }

    const workspaceId = DISPLAY_WS_ID as string;

    // The per-tenant admin bearer satisfies AdminAuth for the acquire POST
    // (which can carry a header). The WS upgrade below relies on Origin
    // (same-origin canvas), NOT this header.
    await context.setExtraHTTPHeaders({
      Authorization: `Bearer ${tenantToken}`,
      // X-Molecule-Org-Id is required by workspace-server TenantGuard for
      // cross-org requests routed through the CP edge; staging-setup exports it.
      // Harmless (and correct) to send on the same-origin tenant box too.
      ...(orgID ? { "X-Molecule-Org-Id": orgID } : {}),
    });

    // 0. Sanity: the workspace must actually be display-enabled, else the
    //    whole gate is meaningless. Hit the availability endpoint first so a
    //    mis-pointed STAGING_DISPLAY_WORKSPACE_ID fails with a precise message
    //    instead of an opaque acquire error.
    const availResp = await page.request.get(
      `${tenantURL}/workspaces/${workspaceId}/display`,
    );
    expect(
      availResp.status(),
      `GET /display for ${workspaceId} should be 200`,
    ).toBe(200);
    const avail = await availResp.json();
    expect(
      avail.available,
      `workspace ${workspaceId} is not display-available (reason=${avail.reason}). ` +
        "STAGING_DISPLAY_WORKSPACE_ID must point at a workspace with " +
        "compute.display.mode == 'desktop-control' AND a live instance_id.",
    ).toBe(true);

    // 1. Acquire display control. The handler returns session_url +
    //    expires_at; session_url embeds the signed token in its #token=
    //    fragment (workspace_display_control.go:signedDisplaySessionURL).
    const acquireResp = await page.request.post(
      `${tenantURL}/workspaces/${workspaceId}/display/control/acquire`,
      { data: { controller: "user", ttl_seconds: 300 } },
    );
    expect(
      acquireResp.status(),
      `acquire should be 200; body: ${await acquireResp.text()}`,
    ).toBe(200);
    const acquire = await acquireResp.json();
    expect(acquire.controller, "controller should be 'user'").toBe("user");
    expect(
      typeof acquire.session_url,
      `acquire response missing session_url: ${JSON.stringify(acquire)}`,
    ).toBe("string");

    // The token rides in the URL fragment (#token=...), never as a query
    // param — confirm the contract the client (DisplayTab.tsx:459-466)
    // depends on so a server-side change to the URL shape fails HERE.
    const sessionUrl: string = acquire.session_url;
    expect(
      sessionUrl,
      `session_url should carry the token in a #token= fragment: ${sessionUrl}`,
    ).toContain("#token=");

    // 2. Open the REAL noVNC WebSocket from inside the page, so the browser
    //    sends Origin: <tenant> and the same-origin-canvas AdminAuth path
    //    accepts the upgrade (a browser WS can't set Authorization). We
    //    navigate to the tenant origin first purely to anchor the Origin
    //    header; we don't need the canvas bundle to hydrate.
    await page.goto(tenantURL, { waitUntil: "domcontentloaded" });

    // Reproduce DisplayTab.tsx:459-466 (displayWebSocketConnection): resolve
    // session_url against the tenant origin, pull the token out of the
    // fragment, strip the fragment, switch http(s)->ws(s). Then connect with
    // the exact subprotocols the canvas uses (DisplayTab.tsx:339).
    const result = await page.evaluate(
      async ({ rawSessionUrl, upgradeTimeoutMs, frameTimeoutMs }) => {
        const u = new URL(rawSessionUrl, window.location.href);
        const token =
          new URLSearchParams(u.hash.replace(/^#/, "")).get("token") ?? "";
        if (!token) {
          return { ok: false, stage: "token-parse", detail: "no #token in session_url" };
        }
        u.hash = "";
        u.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
        const wsUrl = u.toString();

        return await new Promise<{
          ok: boolean;
          stage: string;
          detail: string;
          frameBytes?: number;
          frameKind?: string;
          closeCode?: number;
        }>((resolve) => {
          let upgraded = false;
          let settled = false;
          const finish = (r: {
            ok: boolean;
            stage: string;
            detail: string;
            frameBytes?: number;
            frameKind?: string;
            closeCode?: number;
          }) => {
            if (settled) return;
            settled = true;
            try {
              ws.close();
            } catch {
              /* ignore */
            }
            resolve(r);
          };

          let ws: WebSocket;
          try {
            ws = new WebSocket(wsUrl, [`binary`, `molecule-display-token.${token}`]);
          } catch (e) {
            resolve({ ok: false, stage: "construct", detail: String(e) });
            return;
          }
          ws.binaryType = "arraybuffer";

          const upgradeTimer = setTimeout(() => {
            finish({
              ok: false,
              stage: "upgrade-timeout",
              detail: `WS did not open within ${upgradeTimeoutMs}ms (readyState=${ws.readyState})`,
            });
          }, upgradeTimeoutMs);

          let frameTimer: ReturnType<typeof setTimeout> | null = null;

          ws.onopen = () => {
            upgraded = true;
            clearTimeout(upgradeTimer);
            // Now wait for the first server message. RFB's ProtocolVersion
            // banner is the first thing x11vnc sends; if nothing arrives the
            // tunnel opened but the VNC server behind it is dead.
            frameTimer = setTimeout(() => {
              finish({
                ok: false,
                stage: "frame-timeout",
                detail: `WS upgraded but no framebuffer message within ${frameTimeoutMs}ms`,
              });
            }, frameTimeoutMs);
          };

          ws.onmessage = (ev) => {
            if (frameTimer) clearTimeout(frameTimer);
            let bytes = 0;
            let kind: string = typeof ev.data;
            if (ev.data instanceof ArrayBuffer) {
              bytes = ev.data.byteLength;
              kind = "ArrayBuffer";
            } else if (typeof Blob !== "undefined" && ev.data instanceof Blob) {
              bytes = ev.data.size;
              kind = "Blob";
            } else if (typeof ev.data === "string") {
              bytes = ev.data.length;
              kind = "string";
            }
            finish({
              ok: bytes > 0,
              stage: "frame",
              detail:
                bytes > 0
                  ? "received framebuffer message"
                  : "first message was empty",
              frameBytes: bytes,
              frameKind: kind,
            });
          };

          ws.onclose = (ev) => {
            // A close BEFORE open === failed upgrade (1006 abnormal / 403
            // forbidden surface here). A close AFTER we already saw a frame is
            // benign (our own finish() triggered it).
            if (!upgraded) {
              clearTimeout(upgradeTimer);
              finish({
                ok: false,
                stage: "upgrade-close",
                detail: `WS closed before upgrade (code=${ev.code}, reason="${ev.reason}") — handshake rejected somewhere in edge → ws-proxy → EIC → websockify → x11vnc`,
                closeCode: ev.code,
              });
            }
          };

          ws.onerror = () => {
            if (!upgraded) {
              clearTimeout(upgradeTimer);
              finish({
                ok: false,
                stage: "upgrade-error",
                detail: "WS error before upgrade — proxy chain rejected the handshake",
              });
            }
          };
        });
      },
      {
        rawSessionUrl: sessionUrl,
        upgradeTimeoutMs: WS_UPGRADE_TIMEOUT_MS,
        frameTimeoutMs: FIRST_FRAME_TIMEOUT_MS,
      },
    );

    // 3. Assert the real outcome. No "flaky" escape hatch: each failure stage
    //    names the broken hop so a reviewer can act on it directly.
    expect(
      result.ok,
      `take-control failed at stage="${result.stage}": ${result.detail}` +
        (result.closeCode ? ` (close code ${result.closeCode})` : ""),
    ).toBe(true);
    expect(
      result.stage,
      `expected to reach the 'frame' stage; got '${result.stage}' (${result.detail})`,
    ).toBe("frame");
    expect(
      result.frameBytes ?? 0,
      `framebuffer message should be non-empty (kind=${result.frameKind})`,
    ).toBeGreaterThan(0);
  });
});
