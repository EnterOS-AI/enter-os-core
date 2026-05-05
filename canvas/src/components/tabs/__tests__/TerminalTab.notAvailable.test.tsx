// @vitest-environment jsdom
//
// Pins the "Terminal not available" early-return added 2026-05-05.
//
// Pre-fix: TerminalTab tried to open /ws/terminal/<id> for every
// workspace including external runtimes (which have no shell endpoint).
// The server returned 404, status flipped to "error", user saw
// "Connection failed" with a Reconnect button — reading as a bug
// when really the runtime intentionally has no TTY. Now: when
// data.runtime is in RUNTIMES_WITHOUT_TERMINAL, render a banner +
// big icon instead of mounting xterm/WS.
//
// Pinned branches:
//   1. external runtime → "Terminal not available" banner renders,
//      runtime name surfaces in the body so the user knows WHY.
//   2. external runtime → xterm + WebSocket are NOT initialised.
//      Verified by checking the global WebSocket constructor isn't
//      called.
//   3. claude-code (or any other runtime) → no banner, normal mount
//      proceeds. Pre-fix regression cover.
//   4. data prop omitted (back-compat with any caller that doesn't
//      thread it through) → no early-return, falls through to normal
//      mount. Tested via the absence of the banner.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// xterm + addon-fit are dynamically imported by TerminalTab. Stub them
// so the tests don't pull a 200KB+ dependency just to verify the
// not-available banner. The stubs only matter for the non-banner
// branches; the banner returns BEFORE the dynamic import.
vi.mock("xterm", () => ({
  Terminal: vi.fn().mockImplementation(() => ({
    loadAddon: vi.fn(),
    open: vi.fn(),
    onData: vi.fn(),
    write: vi.fn(),
    dispose: vi.fn(),
    onResize: vi.fn(),
    cols: 80,
    rows: 24,
  })),
}));
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: vi.fn().mockImplementation(() => ({
    fit: vi.fn(),
  })),
}));

// Track WebSocket constructor calls — this is the load-bearing
// assertion for "external doesn't even try to connect".
let wsConstructed = 0;
beforeEach(() => {
  wsConstructed = 0;
  (globalThis as unknown as { WebSocket: unknown }).WebSocket = vi
    .fn()
    .mockImplementation(() => {
      wsConstructed++;
      return {
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        send: vi.fn(),
        close: vi.fn(),
        readyState: 0,
      };
    });
});

import { TerminalTab } from "../TerminalTab";

const externalData = { runtime: "external", status: "online" } as unknown as Parameters<
  typeof TerminalTab
>[0]["data"];

const claudeData = { runtime: "claude-code", status: "online" } as unknown as Parameters<
  typeof TerminalTab
>[0]["data"];

describe("TerminalTab not-available early-return for runtimes without TTY", () => {
  it("external runtime renders the not-available banner with runtime name", () => {
    render(<TerminalTab workspaceId="ws-ext" data={externalData} />);
    expect(screen.getByText(/Terminal not available/i)).not.toBeNull();
    // Runtime name surfaces so user knows WHY there's no terminal.
    expect(screen.getByText(/external/)).not.toBeNull();
  });

  it("external runtime does NOT open a WebSocket", async () => {
    render(<TerminalTab workspaceId="ws-ext" data={externalData} />);
    // Wait a tick for any deferred init (there shouldn't be any, but
    // tolerate a microtask boundary).
    await new Promise((r) => setTimeout(r, 0));
    expect(wsConstructed).toBe(0);
  });

  it("claude-code runtime does NOT render the banner (normal mount)", () => {
    render(<TerminalTab workspaceId="ws-claude" data={claudeData} />);
    expect(screen.queryByText(/Terminal not available/i)).toBeNull();
  });

  it("data prop omitted falls through to normal mount (back-compat)", () => {
    render(<TerminalTab workspaceId="ws-no-data" />);
    expect(screen.queryByText(/Terminal not available/i)).toBeNull();
  });
});
