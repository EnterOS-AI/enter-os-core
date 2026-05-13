// @vitest-environment jsdom
/**
 * Tests for PurchaseSuccessModal component.
 *
 * Covers: no render when no URL params, renders with ?purchase_success=1,
 * portal rendering, item name from &item=, auto-dismiss after 5s,
 * manual dismiss, backdrop click close, Escape key close, URL stripping,
 * focus management.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PurchaseSuccessModal } from "../PurchaseSuccessModal";

// ─── History mock ─────────────────────────────────────────────────────────────
// jsdom's window.history.replaceState throws SecurityError for http://localhost/
// (it normalizes the URL and adds a trailing dot, then fails its own check).
// We intercept replaceState to swallow the error and also update the location
// object directly so window.location.search reflects the current URL params.
const _origReplaceState = window.history.replaceState.bind(window.history);
const _origLocation = window.location;
let _currentHref = "http://localhost/";

// Override window.location with a writable version that tracks our fake href
Object.defineProperty(window, "location", {
  value: {
    get href() { return _currentHref; },
    set href(v: string) { _currentHref = v; },
    get search() {
      const idx = _currentHref.indexOf("?");
      return idx >= 0 ? _currentHref.slice(idx) : "";
    },
    get pathname() {
      const idx = _currentHref.indexOf("?");
      const pathPart = idx >= 0 ? _currentHref.slice(0, idx) : _currentHref;
      return new URL(pathPart).pathname;
    },
    toString: () => _currentHref,
    assign: (url: string) => { _currentHref = url; },
    replace: (url: string) => { _currentHref = url; },
  },
  writable: true,
  configurable: true,
});

(window.history as unknown as Record<string, unknown>).replaceState = function(
  this: History,
  state: unknown,
  title: string,
  url?: string | URL,
) {
  const urlStr = url != null ? String(url) : undefined;
  if (urlStr != null) _currentHref = urlStr;
  try {
    return _origReplaceState.call(this, state, title, url);
  } catch (err) {
    // jsdom throws for http://localhost/ — swallow and rely on our fake location
    return undefined as unknown as void;
  }
} as History["replaceState"];

// ─── Helpers ──────────────────────────────────────────────────────────────────

function replaceUrl(url: string) {
  _currentHref = url;
  try {
    window.history.replaceState(null, "", url);
  } catch {
    // Intercepted above
  }
}

function pushUrl(url: string) {
  replaceUrl(url);
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("PurchaseSuccessModal — render conditions", () => {
  beforeEach(() => {
    replaceUrl("http://localhost/");
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("renders nothing when URL has no purchase_success param", () => {
    replaceUrl("http://localhost/");
    render(<PurchaseSuccessModal />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders nothing on a plain URL", () => {
    replaceUrl("http://localhost/dashboard?foo=bar");
    render(<PurchaseSuccessModal />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders the dialog when ?purchase_success=1 is present", async () => {
    replaceUrl("http://localhost/?purchase_success=1");
    render(<PurchaseSuccessModal />);
    // useEffect fires after mount
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByRole("dialog")).toBeTruthy();
  });

  it("renders the dialog when ?purchase_success=true is present", async () => {
    replaceUrl("http://localhost/?purchase_success=true");
    render(<PurchaseSuccessModal />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByRole("dialog")).toBeTruthy();
  });

  it("renders a portal attached to document.body", async () => {
    replaceUrl("http://localhost/?purchase_success=1");
    render(<PurchaseSuccessModal />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    const dialog = document.body.querySelector('[role="dialog"]');
    expect(dialog).toBeTruthy();
  });

  it("shows the item name when &item= is present", async () => {
    replaceUrl("http://localhost/?purchase_success=1&item=MyAgent");
    render(<PurchaseSuccessModal />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByText("MyAgent")).toBeTruthy();
    expect(screen.getByText("Purchase successful")).toBeTruthy();
  });

  it("shows 'Your new agent' when no item param is present", async () => {
    replaceUrl("http://localhost/?purchase_success=1");
    render(<PurchaseSuccessModal />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByText("Your new agent")).toBeTruthy();
  });

  it("decodes URI-encoded item names", async () => {
    replaceUrl("http://localhost/?purchase_success=1&item=Claude%20Code%20Agent");
    render(<PurchaseSuccessModal />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByText("Claude Code Agent")).toBeTruthy();
  });
});

describe("PurchaseSuccessModal — dismiss", () => {
  beforeEach(() => {
    replaceUrl("http://localhost/?purchase_success=1&item=TestItem");
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("closes the dialog when the close button is clicked", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.getByRole("dialog")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes the dialog when the backdrop is clicked", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.getByRole("dialog")).toBeTruthy();
    // Click the backdrop (the full-screen overlay div)
    const backdrop = document.body.querySelector('[aria-hidden="true"]');
    if (backdrop) fireEvent.click(backdrop);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes on Escape key", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.getByRole("dialog")).toBeTruthy();
    fireEvent.keyDown(window, { key: "Escape" });
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("auto-dismisses after 5 seconds", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.getByRole("dialog")).toBeTruthy();

    // Advance 5 seconds
    act(() => { vi.advanceTimersByTime(5000); });
    await act(async () => { /* flush */ });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("does not auto-dismiss before 5 seconds", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.getByRole("dialog")).toBeTruthy();

    act(() => { vi.advanceTimersByTime(4900); });
    await act(async () => { /* flush */ });
    expect(screen.queryByRole("dialog")).toBeTruthy();
  });
});

describe("PurchaseSuccessModal — URL stripping", () => {
  beforeEach(() => {
    replaceUrl("http://localhost/?purchase_success=1&item=TestItem");
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("strips purchase_success and item params from the URL on mount", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    const url = new URL(window.location.href);
    expect(url.searchParams.get("purchase_success")).toBeNull();
    expect(url.searchParams.get("item")).toBeNull();
  });

  it("uses replaceState (not pushState) so back-button does not re-trigger", async () => {
    const replaceSpy = vi.spyOn(window.history, "replaceState");
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    expect(replaceSpy).toHaveBeenCalled();
  });
});

describe("PurchaseSuccessModal — accessibility", () => {
  beforeEach(() => {
    replaceUrl("http://localhost/?purchase_success=1&item=TestItem");
    vi.useFakeTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("has aria-modal=true on the dialog", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("aria-modal")).toBe("true");
  });

  it("has aria-labelledby pointing to the title", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
    });
    const dialog = screen.getByRole("dialog");
    const labelledby = dialog.getAttribute("aria-labelledby");
    expect(labelledby).toBeTruthy();
    expect(document.getElementById(labelledby!)).toBeTruthy();
    expect(document.getElementById(labelledby!)?.textContent).toMatch(/purchase successful/i);
  });

  it("moves focus to the close button on open", async () => {
    render(<PurchaseSuccessModal />);
    await act(async () => {
      vi.advanceTimersByTime(10);
      // Advance rAF timers as well (ViTest mocks rAF with fake timers)
      vi.advanceTimersByTime(0);
      vi.advanceTimersByTime(0);
    });
    expect(document.activeElement?.textContent).toMatch(/close/i);
  });
});
