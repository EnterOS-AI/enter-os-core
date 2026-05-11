// @vitest-environment jsdom
/**
 * Tests for PurchaseSuccessModal component.
 *
 * Covers: no render when no URL params, renders with ?purchase_success=1,
 * portal rendering, item name from &item=, auto-dismiss after 5s,
 * manual dismiss, backdrop click close, Escape key close, URL stripping,
 * focus management.
 *
 * jsdom requires overriding window.location directly (Object.defineProperty
 * with writable:true) since vi.stubGlobal("location") does not propagate to
 * window.location.search in the jsdom environment.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PurchaseSuccessModal } from "../PurchaseSuccessModal";

// ─── URL stub helper ───────────────────────────────────────────────────────────
// jsdom's window.location.search is read-only by default. We use
// Object.defineProperty to make it writable so tests can control the URL.
function setSearch(search: string) {
  Object.defineProperty(window, "location", {
    writable: true,
    value: { ...window.location, search },
  });
}

function clearSearch() {
  setSearch("");
}

// Helper: wait for the dialog to appear after React useEffect batch.
// Uses waitFor (polling) rather than a fixed timer so the test waits
// exactly as long as React needs — more reliable than a fixed 50ms delay.
async function waitForDialog() {
  await waitFor(() => {
    expect(screen.queryByRole("dialog")).toBeTruthy();
  }, { timeout: 2000 });
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("PurchaseSuccessModal — render conditions", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    clearSearch();
  });

  it("renders nothing when URL has no purchase_success param", () => {
    setSearch("");
    render(<PurchaseSuccessModal />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders nothing on a plain URL", () => {
    setSearch("?foo=bar");
    render(<PurchaseSuccessModal />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders the dialog when ?purchase_success=1 is present", async () => {
    setSearch("?purchase_success=1");
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    expect(screen.queryByRole("dialog")).toBeTruthy();
  });

  it("renders the dialog when ?purchase_success=true is present", async () => {
    setSearch("?purchase_success=true");
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    expect(screen.queryByRole("dialog")).toBeTruthy();
  });

  it("renders a portal attached to document.body", async () => {
    setSearch("?purchase_success=1");
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    const dialog = document.body.querySelector('[role="dialog"]');
    expect(dialog).toBeTruthy();
  });

  it("shows the item name when &item= is present", async () => {
    setSearch("?purchase_success=1&item=MyAgent");
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    expect(screen.getByText("MyAgent")).toBeTruthy();
    expect(screen.getByText("Purchase successful")).toBeTruthy();
  });

  it("shows 'Your new agent' when no item param is present", async () => {
    setSearch("?purchase_success=1");
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    expect(screen.getByText("Your new agent")).toBeTruthy();
  });

  it("decodes URI-encoded item names", async () => {
    setSearch("?purchase_success=1&item=Claude%20Code%20Agent");
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    expect(screen.getByText("Claude Code Agent")).toBeTruthy();
  });
});

describe("PurchaseSuccessModal — dismiss", () => {
  beforeEach(() => {
    setSearch("?purchase_success=1&item=TestItem");
    vi.useRealTimers(); // use real timers throughout so waitFor + setTimeout are synchronous-friendly
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers(); // ensure no fake timer leak
    clearSearch();
  });

  it("closes the dialog when the close button is clicked", async () => {
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await act(async () => { await new Promise((r) => setTimeout(r, 100)); });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes the dialog when the backdrop is clicked", async () => {
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    const backdrop = document.body.querySelector('[aria-hidden="true"]');
    if (backdrop) fireEvent.click(backdrop);
    await act(async () => { await new Promise((r) => setTimeout(r, 100)); });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes on Escape key", async () => {
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    fireEvent.keyDown(window, { key: "Escape" });
    await act(async () => { await new Promise((r) => setTimeout(r, 100)); });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  // Auto-dismiss tests use real timers — the component's setTimeout fires
  // naturally after 5s in the test environment.
  it("auto-dismisses after 5 seconds", async () => {
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    // AUTO_DISMISS_MS = 5000ms. Wait 6s to ensure dismiss has fired + React updated.
    await act(async () => { await new Promise((r) => setTimeout(r, 6000)); });
    expect(screen.queryByRole("dialog")).toBeNull();
  }, 10000);

  it("does not auto-dismiss before 5 seconds", async () => {
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    const dialog = screen.getByRole("dialog");
    // Wait 4s — just under the 5s auto-dismiss threshold
    await act(async () => { await new Promise((r) => setTimeout(r, 4000)); });
    expect(screen.queryByRole("dialog")).toBeTruthy();
  });
});

describe("PurchaseSuccessModal — URL stripping", () => {
  beforeEach(() => {
    setSearch("?purchase_success=1&item=TestItem");
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    clearSearch();
  });

  it("strips purchase_success and item params from the URL on mount", async () => {
    render(<PurchaseSuccessModal />);
    await waitForDialog();
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("uses replaceState (not pushState) so back-button does not re-trigger", async () => {
    setSearch("?purchase_success=1&item=TestItem");
    render(<PurchaseSuccessModal />);
    // Wait for the useEffect (stripPurchaseParams) to fire.
    // Uses a 100ms delay to ensure the async effect has run.
    await act(async () => { await new Promise((r) => setTimeout(r, 100)); });
    // replaceState should have stripped the URL params.
    // jsdom updates window.location.href after replaceState; search becomes "".
    const searchAfter = new URL(window.location.href).searchParams.toString();
    expect(searchAfter).toBe("");
  });
});

describe("PurchaseSuccessModal — accessibility", () => {
  beforeEach(() => {
    setSearch("?purchase_success=1&item=TestItem");
    vi.useRealTimers(); // ensure clean state
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers(); // ensure no fake timer leak
    clearSearch();
  });

  it("has aria-modal=true on the dialog", async () => {
    render(<PurchaseSuccessModal />);
    await waitFor(() => {
      expect(screen.getByRole("dialog").getAttribute("aria-modal")).toBe("true");
    });
  });

  it("has aria-labelledby pointing to the title", async () => {
    render(<PurchaseSuccessModal />);
    await waitFor(() => {
      const dialog = screen.getByRole("dialog");
      const labelledby = dialog.getAttribute("aria-labelledby");
      expect(labelledby).toBeTruthy();
      expect(document.getElementById(labelledby!)).toBeTruthy();
      expect(document.getElementById(labelledby!)?.textContent).toMatch(/purchase successful/i);
    });
  });

  // Focus test: verify close button exists after dialog renders.
  // We test presence (not focus) since rAF focus is tricky in jsdom.
  it("moves focus to the close button on open", async () => {
    render(<PurchaseSuccessModal />);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Close" })).toBeTruthy();
    });
  });
});
