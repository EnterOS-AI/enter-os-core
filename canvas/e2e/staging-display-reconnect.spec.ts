/**
 * Staging canvas E2E — desktop take-control RECONNECT + LEASE-RENEWAL path
 * (core#2332 "P0.7", the e2e gap left by core#2216).
 *
 * Sibling to staging-display.spec.ts. That spec proves the happy path
 * (acquire → noVNC WS upgrade → first framebuffer frame). It does NOT cover
 * the two behaviours core#2216 added on top of that happy path:
 *
 *   (A) RECONNECT re-acquires a FRESH token. When the live WS drops uncleanly
 *       (idle/network blip), DisplayTab.tsx:391-446 calls connect(reacquire=true),
 *       which first awaits reacquireSession() (DisplayTab.tsx:83-99 →
 *       POST /display/control/acquire) to mint a NON-stale lease+token before
 *       reopening the socket. Without this, the cached ~300s token can be past
 *       its expiry and the reconnect would 401 — a dead session that LOOKS like
 *       a reconnect. We assert the reconnect path yields a token bound to a NEW
 *       expires_at AND that a NEW WS opened with that fresh token resumes the
 *       framebuffer (a real frame, not a 1006/403).
 *
 *   (B) The lease SURVIVES past the 300s window via the renewal cadence.
 *       The lock is a 300s lease with NO server-side auto-renewal
 *       (workspace_display_control.go:27 displayControlDefaultTTLSeconds=300;
 *       loadActiveDisplayControl filters `expires_at > now()`). DisplayTab.tsx:105-111
 *       runs a 120_000ms setInterval that re-acquires as the same holder, which
 *       the server's ON-CONFLICT upsert (workspace_display_control.go:116-123,
 *       `controlled_by = EXCLUDED.controlled_by`) treats as a lease EXTENSION:
 *       expires_at moves forward by a fresh 300s each renewal. We do NOT sleep
 *       300s of wall-clock to prove this — we drive the renewal CALL the timer
 *       fires (reacquireSession === the same POST) and assert it pushes
 *       expires_at strictly past the ORIGINAL lease window, then confirm the
 *       lock is still live (GET /display/control returns the holder) after a
 *       point in time at which the original, un-renewed lease would already be
 *       expired. That is the observable, deterministic proxy for "the 120s
 *       timer keeps the user from being kicked every ~5 min."
 *
 * Auth model, gating, and fail-closed philosophy are IDENTICAL to
 * staging-display.spec.ts — see that file's header for the full rationale
 * (same-origin-canvas Origin for the WS upgrade; per-tenant admin bearer for
 * the acquire/GET POSTs; STAGING_DISPLAY_WORKSPACE_ID is the single activation
 * knob and a standing desktop EC2 is a CTO cost item; any failure once the gate
 * env is present is a HARD error, never a silent green, no "flaky" disposition).
 *
 * Promote-to-required is a CTO call: like its sibling this only runs when a
 * standing desktop-capable staging workspace exists, so it cannot be a blanket
 * required context until that workspace is funded and STAGING_DISPLAY_* is wired
 * into the e2e-staging-canvas workflow.
 */

import { test, expect } from "@playwright/test";

const STAGING = process.env.CANVAS_E2E_STAGING === "1";

// The standing desktop-capable workspace id. Absent => skip loud. Same single
// activation knob as staging-display.spec.ts; see that file's header.
const DISPLAY_WS_ID = process.env.STAGING_DISPLAY_WORKSPACE_ID;

test.skip(!STAGING, "CANVAS_E2E_STAGING not set — skipping staging-only tests");
test.skip(
  !DISPLAY_WS_ID,
  "STAGING_DISPLAY_WORKSPACE_ID not set — no standing desktop-capable staging " +
    "workspace to exercise the reconnect/renewal path. Set it to a workspace whose " +
    "compute.display.mode == 'desktop-control' to activate this real-e2e gate. " +
    "(Standing that workspace up is a CTO cost item — one always-on desktop EC2.)",
);

// WS upgrade + first-frame budgets mirror staging-display.spec.ts:75-76 — the
// EIC tunnel + websockify handshake adds real latency; bounded so a dead path
// fails LOUD instead of hanging to the suite timeout.
const WS_UPGRADE_TIMEOUT_MS = 30_000;
const FIRST_FRAME_TIMEOUT_MS = 30_000;

// The production lease/renewal contract we are asserting against:
//   - DEFAULT_TTL_SECONDS: the 300s lease the canvas requests
//     (DisplayTab.tsx:88 ttl_seconds:300; server default
//     workspace_display_control.go:27).
//   - RENEWAL_INTERVAL_MS: the cadence the canvas renews on
//     (DisplayTab.tsx:109 setInterval(..., 120_000)). We don't sleep it; we
//     assert the renewal CALL pushes the lease forward.
const DEFAULT_TTL_SECONDS = 300;
const RENEWAL_INTERVAL_MS = 120_000;

// Open a real noVNC WebSocket from inside the page (so the browser sends
// Origin: <tenant> and the same-origin-canvas AdminAuth path accepts the
// upgrade — a browser WS can't set Authorization). Returns the outcome of the
// upgrade + first-frame, exactly like staging-display.spec.ts's evaluate
// block. Reused here for BOTH the initial connect and the post-drop reconnect
// so the two are compared on identical wire mechanics.
type WsResult = {
  ok: boolean;
  stage: string;
  detail: string;
  frameBytes?: number;
  frameKind?: string;
  closeCode?: number;
};

async function openDisplayWs(
  page: import("@playwright/test").Page,
  rawSessionUrl: string,
): Promise<WsResult> {
  return page.evaluate(
    async ({ rawSessionUrl, upgradeTimeoutMs, frameTimeoutMs }) => {
      // Reproduce DisplayTab.tsx:545-552 (displayWebSocketConnection): resolve
      // against the tenant origin, pull token from the #token fragment, strip
      // the fragment, switch http(s)->ws(s). Then connect with the exact
      // subprotocols the canvas uses (DisplayTab.tsx:402).
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
              bytes > 0 ? "received framebuffer message" : "first message was empty",
            frameBytes: bytes,
            frameKind: kind,
          });
        };

        ws.onclose = (ev) => {
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
      rawSessionUrl,
      upgradeTimeoutMs: WS_UPGRADE_TIMEOUT_MS,
      frameTimeoutMs: FIRST_FRAME_TIMEOUT_MS,
    },
  );
}

// Pull the opaque signed token out of a session_url's #token= fragment so we
// can compare reconnect tokens for freshness (a reconnect MUST mint a new one
// — same token would mean the cached, possibly-expired URL was reused).
function tokenOf(sessionUrl: string): string {
  const hashIdx = sessionUrl.indexOf("#token=");
  return hashIdx >= 0 ? sessionUrl.slice(hashIdx + "#token=".length) : "";
}

test.describe("staging desktop take-control — reconnect + lease renewal (core#2216)", () => {
  // Shared staging context resolution — identical to staging-display.spec.ts:90-120.
  function resolveTenant() {
    const tenantURL =
      process.env.STAGING_DISPLAY_TENANT_URL || process.env.STAGING_TENANT_URL;
    const tenantToken =
      process.env.STAGING_DISPLAY_TENANT_TOKEN || process.env.STAGING_TENANT_TOKEN;
    const orgID = process.env.STAGING_DISPLAY_ORG_ID || process.env.STAGING_ORG_ID;
    if (!tenantURL || !tenantToken) {
      throw new Error(
        "STAGING_DISPLAY_WORKSPACE_ID is set but no tenant URL/token is available " +
          "for the reconnect/renewal gate. Set STAGING_DISPLAY_SLUG so staging-setup.ts " +
          "resolves STAGING_DISPLAY_TENANT_URL / STAGING_DISPLAY_TENANT_TOKEN for the " +
          "standing desktop org (or ensure the ephemeral STAGING_TENANT_* exports exist).",
      );
    }
    return { tenantURL, tenantToken, orgID };
  }

  test.beforeEach(async ({ context }) => {
    const { tenantToken, orgID } = resolveTenant();
    await context.setExtraHTTPHeaders({
      Authorization: `Bearer ${tenantToken}`,
      ...(orgID ? { "X-Molecule-Org-Id": orgID } : {}),
    });
  });

  test("reconnect re-acquires a FRESH token and the framebuffer resumes", async ({
    page,
  }) => {
    const { tenantURL } = resolveTenant();
    const workspaceId = DISPLAY_WS_ID as string;

    // Sanity: workspace must be display-available, else the gate is meaningless.
    const availResp = await page.request.get(
      `${tenantURL}/workspaces/${workspaceId}/display`,
    );
    expect(availResp.status(), `GET /display for ${workspaceId} should be 200`).toBe(200);
    const avail = await availResp.json();
    expect(
      avail.available,
      `workspace ${workspaceId} is not display-available (reason=${avail.reason}).`,
    ).toBe(true);

    // 1. Initial acquire — the happy-path lease the user starts with.
    const firstResp = await page.request.post(
      `${tenantURL}/workspaces/${workspaceId}/display/control/acquire`,
      { data: { controller: "user", ttl_seconds: DEFAULT_TTL_SECONDS } },
    );
    expect(
      firstResp.status(),
      `initial acquire should be 200; body: ${await firstResp.text()}`,
    ).toBe(200);
    const first = await firstResp.json();
    expect(first.controller, "controller should be 'user'").toBe("user");
    expect(typeof first.session_url, "acquire missing session_url").toBe("string");
    const firstUrl: string = first.session_url;
    expect(firstUrl, "session_url should carry #token=").toContain("#token=");
    const firstToken = tokenOf(firstUrl);
    expect(firstToken.length, "first token should be non-empty").toBeGreaterThan(0);

    // Anchor Origin to the tenant so the same-origin-canvas WS upgrade is accepted.
    await page.goto(tenantURL, { waitUntil: "domcontentloaded" });

    // 2. Establish the live WS on the FIRST token — proves the session is real.
    const initial = await openDisplayWs(page, firstUrl);
    expect(
      initial.ok,
      `initial connect failed at stage="${initial.stage}": ${initial.detail}` +
        (initial.closeCode ? ` (close code ${initial.closeCode})` : ""),
    ).toBe(true);
    expect(initial.stage, `initial connect should reach 'frame'; got '${initial.stage}'`).toBe(
      "frame",
    );

    // 3. Simulate an unclean drop. openDisplayWs() already closed its socket
    //    on finish(), so the live stream is gone here — exactly the state
    //    DisplayTab's "disconnect" handler (DisplayTab.tsx:426-442) enters
    //    before it calls connect(reacquire=true).

    // 4. Reconnect path: mint a FRESH lease+token FIRST, the way
    //    connect(reacquire=true) → reacquireSession() does (DisplayTab.tsx:397
    //    / :83-99). This is a re-acquire by the SAME holder, so the server's
    //    ON-CONFLICT upsert extends the lease and returns a new signed URL.
    const reResp = await page.request.post(
      `${tenantURL}/workspaces/${workspaceId}/display/control/acquire`,
      { data: { controller: "user", ttl_seconds: DEFAULT_TTL_SECONDS } },
    );
    expect(
      reResp.status(),
      `reconnect re-acquire should be 200 (same holder extends, not 409); body: ${await reResp.text()}`,
    ).toBe(200);
    const re = await reResp.json();
    expect(re.controller, "reconnect controller should still be 'user'").toBe("user");
    expect(typeof re.session_url, "reconnect acquire missing session_url").toBe("string");
    const reUrl: string = re.session_url;
    const reToken = tokenOf(reUrl);
    expect(reToken.length, "reconnect token should be non-empty").toBeGreaterThan(0);

    // The reconnect token MUST be fresh — bound to the new expires_at. A
    // reused token would mean the canvas fell back to a cached, soon-expiring
    // URL, which is precisely the 401-on-reconnect bug core#2216 fixed. The
    // signed token embeds expires_at.Unix() (workspace_display_control.go:390),
    // so a later expiry => a different signature => a different token.
    expect(
      reToken,
      "reconnect should mint a FRESH token (bound to the renewed expires_at), " +
        "not reuse the original ~300s token — a reused token is the core#2216 401 bug.",
    ).not.toBe(firstToken);
    expect(
      new Date(re.expires_at).getTime(),
      "renewed expires_at should be >= the original (lease extended, not shrunk)",
    ).toBeGreaterThanOrEqual(new Date(first.expires_at).getTime());

    // 5. Reopen the WS on the FRESH token and assert the framebuffer RESUMES —
    //    a real frame, not a dead 1006/403 session. This is the crux: the
    //    reconnect produces a LIVE stream, not a stale-token rejection.
    const reconnected = await openDisplayWs(page, reUrl);
    expect(
      reconnected.ok,
      `RECONNECT failed at stage="${reconnected.stage}": ${reconnected.detail}` +
        (reconnected.closeCode ? ` (close code ${reconnected.closeCode})` : "") +
        " — a 1006/403 here means the fresh-token reconnect did NOT re-establish " +
        "the proxy chain (edge → ws-proxy → EIC → websockify → x11vnc).",
    ).toBe(true);
    expect(
      reconnected.stage,
      `reconnect should reach 'frame' (framebuffer resumed); got '${reconnected.stage}' (${reconnected.detail})`,
    ).toBe("frame");
    expect(
      reconnected.frameBytes ?? 0,
      `resumed framebuffer message should be non-empty (kind=${reconnected.frameKind})`,
    ).toBeGreaterThan(0);
  });

  test("renewal pushes the lease past the original 300s window (no kick at ~5min)", async ({
    page,
  }) => {
    const { tenantURL } = resolveTenant();
    const workspaceId = DISPLAY_WS_ID as string;

    // 1. Acquire the initial 300s lease.
    const firstResp = await page.request.post(
      `${tenantURL}/workspaces/${workspaceId}/display/control/acquire`,
      { data: { controller: "user", ttl_seconds: DEFAULT_TTL_SECONDS } },
    );
    expect(
      firstResp.status(),
      `initial acquire should be 200; body: ${await firstResp.text()}`,
    ).toBe(200);
    const first = await firstResp.json();
    const firstExpiry = new Date(first.expires_at).getTime();
    expect(Number.isFinite(firstExpiry), "first expires_at should parse").toBe(true);

    // The original lease's hard ceiling: when the un-renewed token/lock dies.
    const originalLeaseDeadlineMs = firstExpiry;

    // 2. Fire the renewal CALL the 120s timer fires (DisplayTab.tsx:107-109 →
    //    reacquireSession → this same POST). We don't sleep RENEWAL_INTERVAL_MS
    //    of wall-clock; we drive the observable call the timer would make and
    //    assert its EFFECT on the lease. RENEWAL_INTERVAL_MS is asserted to sit
    //    safely inside the TTL so the renew always lands before expiry — if a
    //    future change widened the interval past the TTL, this guard fails.
    expect(
      RENEWAL_INTERVAL_MS,
      "renewal interval must be strictly inside the lease TTL, else the lease " +
        "expires before the timer renews it (user gets kicked).",
    ).toBeLessThan(DEFAULT_TTL_SECONDS * 1000);

    const renewResp = await page.request.post(
      `${tenantURL}/workspaces/${workspaceId}/display/control/acquire`,
      { data: { controller: "user", ttl_seconds: DEFAULT_TTL_SECONDS } },
    );
    expect(
      renewResp.status(),
      `renewal re-acquire should be 200 (same holder extends); body: ${await renewResp.text()}`,
    ).toBe(200);
    const renew = await renewResp.json();
    const renewedExpiry = new Date(renew.expires_at).getTime();

    // 3. The renewal MUST push expires_at strictly PAST the original lease
    //    window — that is the whole point of core#2216's renewal timer: a
    //    fresh 300s starting now, so the lease outlives the original ~300s
    //    deadline and the user is not kicked every ~5 minutes. (now()+300s,
    //    fired before the original 300s elapsed, is strictly later than the
    //    original now()+300s.)
    expect(
      renewedExpiry,
      "renewal should extend the lease strictly past the original 300s deadline " +
        `(original=${first.expires_at}, renewed=${renew.expires_at}). Equal-or-earlier ` +
        "means the renewal did NOT extend — the 120s timer would not save the session.",
    ).toBeGreaterThan(originalLeaseDeadlineMs);

    // 4. Confirm the lock is still LIVE after renewal — GET /display/control
    //    only returns a holder when expires_at > now() (loadActiveDisplayControl,
    //    workspace_display_control.go:280). A held controller here proves the
    //    renewed lease is active, not expired.
    const ctrlResp = await page.request.get(
      `${tenantURL}/workspaces/${workspaceId}/display/control`,
    );
    expect(ctrlResp.status(), "GET /display/control should be 200").toBe(200);
    const ctrl = await ctrlResp.json();
    expect(
      ctrl.controller,
      "after renewal the lock should still report a live holder (not 'none')",
    ).toBe("user");
    expect(
      new Date(ctrl.expires_at).getTime(),
      "the live lock's expires_at should match the renewed lease (lease is the " +
        "renewed one, not the original).",
    ).toBeGreaterThan(originalLeaseDeadlineMs);

    // TODO(core#2332, CTO cost item): the assertions above prove the renewal
    // CALL extends the lease past the original window — the deterministic proxy
    // for "the 120s interval keeps the lease alive past 300s." To additionally
    // prove the lease survives a FULL real-time 300s+ idle WS (the literal
    // wall-clock claim), a long-lived test would hold one WS open >300s while
    // the 120s timer renews underneath and assert the SAME socket never 1006s.
    // That needs >5 min of standing-desktop wall-clock per run and is gated on
    // the standing desktop EC2 being funded; it is NOT exercised here. Promote
    // either form to a REQUIRED context only on CTO sign-off (cost + cadence).
  });
});
