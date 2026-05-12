// @vitest-environment jsdom
/**
 * FilesTab: NotAvailablePanel + FilesToolbar coverage.
 *
 * NotAvailablePanel: pure presentational component — renders a "feature not
 * available" placeholder for external-runtime workspaces.
 * FilesToolbar: pure props-driven component — directory selector, file count,
 * action buttons (New, Upload, Export, Clear, Refresh) with correct aria-labels.
 *
 * No @testing-library/jest-dom import — use textContent / className /
 * getAttribute checks to avoid "expect is not defined" errors.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import React from "react";

import { FilesToolbar } from "../FilesToolbar";
import { NotAvailablePanel } from "../NotAvailablePanel";

// ─── afterEach ─────────────────────────────────────────────────────────────────

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── NotAvailablePanel ─────────────────────────────────────────────────────────

describe("NotAvailablePanel", () => {
  it("renders heading 'Files not available'", () => {
    const { container } = render(<NotAvailablePanel runtime="external" />);
    expect(container.textContent).toContain("Files not available");
  });

  it("renders the runtime name in monospace", () => {
    const { container } = render(<NotAvailablePanel runtime="external" />);
    expect(container.textContent).toContain("external");
    const spans = container.querySelectorAll("span");
    const monoSpans = Array.from(spans).filter(
      (s) => s.className && s.className.includes("font-mono"),
    );
    expect(monoSpans.length).toBeGreaterThan(0);
  });

  it("renders a Chat tab hint in description", () => {
    const { container } = render(<NotAvailablePanel runtime="remote-agent" />);
    expect(container.textContent).toContain("Chat tab");
  });

  it("SVG icon has aria-hidden=true", () => {
    const { container } = render(<NotAvailablePanel runtime="external" />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
  });

  it("renders without crashing for any runtime string", () => {
    const { container } = render(<NotAvailablePanel runtime="unknown-runtime" />);
    expect(container.textContent).toContain("unknown-runtime");
  });

  it("applies the correct layout classes to root div", () => {
    const { container } = render(<NotAvailablePanel runtime="external" />);
    const root = container.firstElementChild as HTMLElement;
    expect(root.className).toContain("flex");
    expect(root.className).toContain("flex-col");
    expect(root.className).toContain("items-center");
  });
});

// ─── FilesToolbar ───────────────────────────────────────────────────────────────

describe("FilesToolbar", () => {
  const noop = vi.fn();

  function renderToolbar(props: Partial<React.ComponentProps<typeof FilesToolbar>> = {}) {
    return render(
      <FilesToolbar
        root="/configs"
        setRoot={noop}
        fileCount={0}
        onNewFile={noop}
        onUpload={noop}
        onDownloadAll={noop}
        onClearAll={noop}
        onRefresh={noop}
        {...props}
      />,
    );
  }

  it("renders the directory selector with correct aria-label", () => {
    const { container } = renderToolbar();
    const select = container.querySelector("select");
    expect(select?.getAttribute("aria-label")).toBe("File root directory");
  });

  it("directory selector has all four options", () => {
    const { container } = renderToolbar();
    const select = container.querySelector("select") as HTMLSelectElement;
    const options = Array.from(select?.options ?? []);
    const values = options.map((o) => o.value);
    expect(values).toContain("/configs");
    expect(values).toContain("/home");
    expect(values).toContain("/workspace");
    expect(values).toContain("/plugins");
  });

  it("calls setRoot when directory changes", () => {
    const setRoot = vi.fn();
    const { container } = renderToolbar({ setRoot });
    const select = container.querySelector("select") as HTMLSelectElement;
    select.value = "/home";
    select.dispatchEvent(new Event("change", { bubbles: true }));
    expect(setRoot).toHaveBeenCalledWith("/home");
  });

  it("displays the file count", () => {
    const { container } = renderToolbar({ fileCount: 42 });
    expect(container.textContent).toContain("42 files");
  });

  it("shows New + Upload + Clear buttons for /configs", () => {
    const { container } = renderToolbar({ root: "/configs" });
    const texts = Array.from(container.querySelectorAll("button")).map(
      (b) => b.textContent?.trim(),
    );
    expect(texts).toContain("+ New");
    expect(texts).toContain("Upload");
    expect(texts).toContain("Clear");
    expect(texts).toContain("Export");
    expect(texts).toContain("↻");
  });

  it("hides New + Upload + Clear for /workspace", () => {
    const { container } = renderToolbar({ root: "/workspace" });
    const texts = Array.from(container.querySelectorAll("button")).map(
      (b) => b.textContent?.trim(),
    );
    expect(texts).not.toContain("+ New");
    expect(texts).not.toContain("Upload");
    expect(texts).not.toContain("Clear");
    expect(texts).toContain("Export");
  });

  it("hides New + Upload + Clear for /home", () => {
    const { container } = renderToolbar({ root: "/home" });
    const texts = Array.from(container.querySelectorAll("button")).map(
      (b) => b.textContent?.trim(),
    );
    expect(texts).not.toContain("+ New");
    expect(texts).not.toContain("Upload");
    expect(texts).not.toContain("Clear");
  });

  it("hides New + Upload + Clear for /plugins", () => {
    const { container } = renderToolbar({ root: "/plugins" });
    const texts = Array.from(container.querySelectorAll("button")).map(
      (b) => b.textContent?.trim(),
    );
    expect(texts).not.toContain("+ New");
    expect(texts).not.toContain("Upload");
    expect(texts).not.toContain("Clear");
  });

  it("New button has correct aria-label", () => {
    const { container } = renderToolbar({ root: "/configs" });
    const newBtn = container.querySelector('button[aria-label="Create new file"]');
    expect(newBtn?.textContent?.trim()).toBe("+ New");
  });

  it("Export button has correct aria-label", () => {
    const { container } = renderToolbar();
    const exportBtn = container.querySelector('button[aria-label="Download all files"]');
    expect(exportBtn?.textContent?.trim()).toBe("Export");
  });

  it("Clear button has correct aria-label", () => {
    const { container } = renderToolbar({ root: "/configs" });
    const clearBtn = container.querySelector('button[aria-label="Delete all files"]');
    expect(clearBtn?.textContent?.trim()).toBe("Clear");
  });

  it("Refresh button has correct aria-label", () => {
    const { container } = renderToolbar();
    const refreshBtn = container.querySelector('button[aria-label="Refresh file list"]');
    expect(refreshBtn?.textContent?.trim()).toBe("↻");
  });

  it("calls onNewFile when New button is clicked", () => {
    const onNewFile = vi.fn();
    const { container } = renderToolbar({ root: "/configs", onNewFile });
    container.querySelector('button[aria-label="Create new file"]')!.click();
    expect(onNewFile).toHaveBeenCalledTimes(1);
  });

  it("calls onDownloadAll when Export button is clicked", () => {
    const onDownloadAll = vi.fn();
    const { container } = renderToolbar({ onDownloadAll });
    container.querySelector('button[aria-label="Download all files"]')!.click();
    expect(onDownloadAll).toHaveBeenCalledTimes(1);
  });

  it("calls onClearAll when Clear button is clicked", () => {
    const onClearAll = vi.fn();
    const { container } = renderToolbar({ root: "/configs", onClearAll });
    container.querySelector('button[aria-label="Delete all files"]')!.click();
    expect(onClearAll).toHaveBeenCalledTimes(1);
  });

  it("calls onRefresh when Refresh button is clicked", () => {
    const onRefresh = vi.fn();
    const { container } = renderToolbar({ onRefresh });
    container.querySelector('button[aria-label="Refresh file list"]')!.click();
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });
});
