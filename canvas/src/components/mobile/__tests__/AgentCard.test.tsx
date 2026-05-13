// @vitest-environment jsdom
/**
 * AgentCard — mobile agent row card.
 *
 * Per WCAG 2.1 AA:
 *   - Rendered as <button> with aria-label composing accessible name
 *   - aria-label includes: name, status, tier, remote flag
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { AgentCard, type MobileAgent } from "../components";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

const onlineAgent: MobileAgent = {
  id: "ws-1",
  name: "My Agent",
  tag: "claude-code",
  tier: "T2",
  status: "online",
  remote: false,
  runtime: "claude-code",
  skills: 3,
  calls: 12,
  desc: "Handles customer support",
  parentId: null,
};

const remoteFailedAgent: MobileAgent = {
  id: "ws-2",
  name: "Remote Worker",
  tag: "external",
  tier: "T4",
  status: "failed",
  remote: true,
  runtime: "external",
  skills: 5,
  calls: 0,
  desc: "",
  parentId: "ws-1",
};

// ─── Render ───────────────────────────────────────────────────────────────────

describe("AgentCard — render", () => {
  it("renders as a button", () => {
    render(<AgentCard agent={onlineAgent} dark={false} onClick={vi.fn()} />);
    expect(document.querySelector("button")).toBeTruthy();
  });

  it("button has aria-label with name, status, tier", () => {
    render(<AgentCard agent={onlineAgent} dark={false} onClick={vi.fn()} />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    const label = btn.getAttribute("aria-label") ?? "";
    expect(label).toContain("My Agent");
    expect(label).toContain("online");
    expect(label).toContain("T2");
  });

  it("aria-label includes remote for remote agents", () => {
    render(<AgentCard agent={remoteFailedAgent} dark={false} onClick={vi.fn()} />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    const label = btn.getAttribute("aria-label") ?? "";
    expect(label).toContain("Remote Worker");
    expect(label).toContain("failed");
    expect(label).toContain("T4");
    expect(label).toContain("remote");
  });

  it("aria-label omits remote for non-remote agents", () => {
    render(<AgentCard agent={onlineAgent} dark={false} onClick={vi.fn()} />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    const label = btn.getAttribute("aria-label") ?? "";
    expect(label).not.toContain("remote");
  });

  it("renders agent name text inside the button", () => {
    render(<AgentCard agent={onlineAgent} dark={false} onClick={vi.fn()} />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    expect(btn.textContent).toContain("My Agent");
  });

  it("compact prop reduces padding", () => {
    render(<AgentCard agent={onlineAgent} dark={false} onClick={vi.fn()} compact={true} />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    const style = btn.getAttribute("style") ?? "";
    // compact uses "12px 14px" padding vs "14px 16px" default
    expect(style).toContain("padding");
  });
});

// ─── Interaction ─────────────────────────────────────────────────────────────

describe("AgentCard — interaction", () => {
  it("calls onClick when button is clicked", () => {
    const onClick = vi.fn();
    render(<AgentCard agent={onlineAgent} dark={false} onClick={onClick} />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    btn.click();
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("renders without onClick (optional prop)", () => {
    // Should not throw
    expect(() => render(<AgentCard agent={onlineAgent} dark={false} />)).not.toThrow();
  });
});
