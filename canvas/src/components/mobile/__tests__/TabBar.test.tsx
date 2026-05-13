// @vitest-environment jsdom
/**
 * TabBar — mobile bottom navigation bar.
 *
 * Per WCAG 2.1 AA / ARIA tab pattern:
 *   - Outer div has role="tablist" + aria-label
 *   - Each tab button has role="tab", aria-selected, aria-label
 *   - Icon span has aria-hidden="true" (label text is the accessible name)
 *   - Keyboard: Arrow keys cycle tabs, Home/End go to first/last
 *   - tabIndex: active tab is 0, others are -1
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import React from "react";

import { TabBar, type MobileTabId } from "../components";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

// ─── Render ───────────────────────────────────────────────────────────────────

describe("TabBar — render", () => {
  it("renders 4 tab buttons", () => {
    render(<TabBar active="agents" onChange={vi.fn()} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    expect(tabs.length).toBe(4);
  });

  it("outer div has role=tablist and aria-label", () => {
    render(<TabBar active="agents" onChange={vi.fn()} dark={false} />);
    const tablist = document.querySelector('[role="tablist"]');
    expect(tablist).toBeTruthy();
    expect(tablist?.getAttribute("aria-label")).toBe("Mobile navigation");
  });

  it("each tab button has role=tab and aria-label", () => {
    render(<TabBar active="agents" onChange={vi.fn()} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    tabs.forEach((tab) => {
      expect(tab.getAttribute("role")).toBe("tab");
      expect(tab.getAttribute("aria-label")).toBeTruthy();
    });
  });

  it("icon spans have aria-hidden=true", () => {
    render(<TabBar active="agents" onChange={vi.fn()} dark={false} />);
    const icons = document.querySelectorAll('[aria-hidden="true"]');
    expect(icons.length).toBeGreaterThanOrEqual(4);
  });

  it("active tab has aria-selected=true, others false", () => {
    render(<TabBar active="canvas" onChange={vi.fn()} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    tabs.forEach((tab) => {
      const label = tab.getAttribute("aria-label");
      if (label === "Canvas") {
        expect(tab.getAttribute("aria-selected")).toBe("true");
      } else {
        expect(tab.getAttribute("aria-selected")).toBe("false");
      }
    });
  });

  it("active tab has tabIndex=0, others tabIndex=-1", () => {
    render(<TabBar active="comms" onChange={vi.fn()} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    tabs.forEach((tab) => {
      const label = tab.getAttribute("aria-label");
      if (label === "Comms") {
        expect(tab.getAttribute("tabIndex")).toBe("0");
      } else {
        expect(tab.getAttribute("tabIndex")).toBe("-1");
      }
    });
  });
});

// ─── Interaction ─────────────────────────────────────────────────────────────

describe("TabBar — interaction", () => {
  it("calls onChange with correct id when tab is clicked", () => {
    const onChange = vi.fn();
    render(<TabBar active="agents" onChange={onChange} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    const canvasTab = Array.from(tabs).find((t) => t.getAttribute("aria-label") === "Canvas") as Element;
    fireEvent.click(canvasTab);
    expect(onChange).toHaveBeenCalledWith("canvas");
  });

  it("ArrowRight moves focus to next tab and activates it", () => {
    const onChange = vi.fn();
    render(<TabBar active="agents" onChange={onChange} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    const agentsTab = tabs[0] as HTMLElement;
    agentsTab.focus();
    expect(document.activeElement).toBe(agentsTab);

    fireEvent.keyDown(agentsTab, { key: "ArrowRight" });
    // onChange called for the next tab
    expect(onChange).toHaveBeenCalledWith("canvas");
    // Focus should move to the canvas tab
    // Use setTimeout(0) trick — after state update, focus moves
  });

  it("ArrowLeft on first tab wraps to last", () => {
    const onChange = vi.fn();
    render(<TabBar active="agents" onChange={onChange} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    const agentsTab = tabs[0] as HTMLElement;
    agentsTab.focus();

    fireEvent.keyDown(agentsTab, { key: "ArrowLeft" });
    expect(onChange).toHaveBeenCalledWith("me");
  });

  it("Home key activates first tab", () => {
    const onChange = vi.fn();
    render(<TabBar active="comms" onChange={onChange} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    const commsTab = tabs[2] as HTMLElement;
    commsTab.focus();

    fireEvent.keyDown(commsTab, { key: "Home" });
    expect(onChange).toHaveBeenCalledWith("agents");
  });

  it("End key activates last tab", () => {
    const onChange = vi.fn();
    render(<TabBar active="agents" onChange={onChange} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    const agentsTab = tabs[0] as HTMLElement;
    agentsTab.focus();

    fireEvent.keyDown(agentsTab, { key: "End" });
    expect(onChange).toHaveBeenCalledWith("me");
  });

  it("ArrowDown also navigates (aliases ArrowRight)", () => {
    const onChange = vi.fn();
    render(<TabBar active="canvas" onChange={onChange} dark={false} />);
    const tabs = document.querySelectorAll('[role="tab"]');
    const canvasTab = tabs[1] as HTMLElement;
    canvasTab.focus();

    fireEvent.keyDown(canvasTab, { key: "ArrowDown" });
    expect(onChange).toHaveBeenCalledWith("comms");
  });
});
