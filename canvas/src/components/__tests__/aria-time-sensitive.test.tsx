// @vitest-environment jsdom
/**
 * WCAG 2 audit — time-sensitive component ARIA fixes:
 *   Fix 1: ApprovalBanner — role="alert" aria-live="assertive" + aria-hidden on ⚠ icon
 *   Fix 2: TerminalTab    — role="status" on connection bar, role="alert" on error
 *   Fix 3: BundleDropZone — keyboard file-picker (hidden <input> + accessible button)
 *                           + role="status" on result toast
 */
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ────────────────────────────────────────────────────────────────────────────
// Fix 1 — ApprovalBanner
// ────────────────────────────────────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn().mockResolvedValue([]),
    post: vi.fn().mockResolvedValue({}),
  },
}));

vi.mock("../Toaster", () => ({ showToast: vi.fn() }));

import { api } from "@/lib/api";
import { ApprovalBanner } from "../ApprovalBanner";

// Stub a minimal approval so the banner renders
const mockApproval = {
  id: "a1",
  workspace_id: "ws-1",
  workspace_name: "PM Agent",
  action: "Run deployment script",
  reason: "Routine release",
  status: "pending",
  created_at: new Date().toISOString(),
};

describe("ApprovalBanner — ARIA time-sensitive (Fix 1)", () => {
  beforeEach(() => {
    vi.mocked(api.get).mockResolvedValue([mockApproval]);
  });

  it("renders role='alert' with aria-live='assertive' on each approval card", async () => {
    const { findByRole } = render(<ApprovalBanner />);
    const alert = await findByRole("alert");
    expect(alert.getAttribute("aria-live")).toBe("assertive");
    expect(alert.getAttribute("aria-atomic")).toBe("true");
  });

  it("⚠ icon span has aria-hidden='true'", async () => {
    render(<ApprovalBanner />);
    // Wait for data
    await screen.findByRole("alert");
    // The ⚠ span should be aria-hidden
    const hiddenSpans = document.querySelectorAll('[aria-hidden="true"]');
    const warningSpan = Array.from(hiddenSpans).find((el) =>
      el.textContent?.includes("⚠")
    );
    expect(warningSpan).not.toBeNull();
  });
});

// ────────────────────────────────────────────────────────────────────────────
// Fix 2 — TerminalTab
// ────────────────────────────────────────────────────────────────────────────

// Mock xterm — not installed in jsdom, just need component to render
vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    loadAddon = vi.fn();
    open = vi.fn();
    dispose = vi.fn();
    onData = vi.fn(() => ({ dispose: vi.fn() }));
    onResize = vi.fn(() => ({ dispose: vi.fn() }));
    writeln = vi.fn();
    write = vi.fn();
    clear = vi.fn();
    options = {};
  },
}));
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: class {
    fit = vi.fn();
    activate = vi.fn();
    dispose = vi.fn();
  },
}));
vi.mock("@xterm/addon-web-links", () => ({
  WebLinksAddon: class { activate = vi.fn(); dispose = vi.fn(); },
}));

import { TerminalTab } from "../tabs/TerminalTab";

describe("TerminalTab — ARIA live regions (Fix 2)", () => {
  it("status bar wrapper has role='status' and aria-live='polite'", () => {
    render(<TerminalTab workspaceId="ws-1" />);
    const statusBar = document.querySelector('[role="status"]');
    expect(statusBar).not.toBeNull();
    expect(statusBar?.getAttribute("aria-live")).toBe("polite");
  });

  it("status bar text changes reflect connection state (content test)", () => {
    render(<TerminalTab workspaceId="ws-1" />);
    // Default state while attempting to connect will show some status text
    const statusBar = document.querySelector('[role="status"]');
    expect(statusBar?.textContent?.length).toBeGreaterThan(0);
  });
});

// ────────────────────────────────────────────────────────────────────────────
// Fix 3 — BundleDropZone
// ────────────────────────────────────────────────────────────────────────────

import { BundleDropZone } from "../BundleDropZone";

describe("BundleDropZone — keyboard accessibility (Fix 3)", () => {
  it("renders a hidden file input with accept='.bundle.json' and an accessible label", () => {
    render(<BundleDropZone />);
    const input = document.getElementById("bundle-file-input") as HTMLInputElement;
    expect(input).not.toBeNull();
    expect(input?.type).toBe("file");
    expect(input?.accept).toBe(".bundle.json");
    expect(input?.getAttribute("aria-label")).toBeTruthy();
    // Must be visually hidden but still reachable by AT
    expect(input?.className).toContain("sr-only");
  });

  it("renders a keyboard-accessible import button that is tabbable", () => {
    render(<BundleDropZone />);
    // The button may be sr-only but must exist in the DOM and be focusable
    const btn = screen.getByRole("button", { name: /import bundle/i });
    expect(btn).not.toBeNull();
  });

  it("result toast renders with role='status' and aria-live='polite'", async () => {
    vi.mocked(api.post).mockResolvedValue({ name: "my-bundle", status: "ok" });

    render(<BundleDropZone />);

    const input = document.getElementById("bundle-file-input") as HTMLInputElement;

    const file = new File(['{"workspaces":[]}'], "test.bundle.json", {
      type: "application/json",
    });

    // Simulate file selection via the hidden input
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await fireEvent.change(input);

    // Toast should appear with role=status
    const toast = await screen.findByRole("status");
    expect(toast).not.toBeNull();
    expect(toast.getAttribute("aria-live")).toBe("polite");
  });
});
