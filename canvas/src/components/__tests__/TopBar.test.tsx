// @vitest-environment jsdom
/**
 * Tests for TopBar component.
 *
 * Covers: renders header, logo, canvas name, "+ New Agent" button,
 * SettingsButton integration, custom canvasName prop.
 */
import React from "react";
import { render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TopBar } from "../canvas/TopBar";

afterEach(cleanup);

// ─── Mock SettingsButton ───────────────────────────────────────────────────────

vi.mock("../settings/SettingsButton", () => ({
  SettingsButton: vi.fn(() => <button aria-label="Settings">⚙</button>),
}));

describe("TopBar — render", () => {
  // Scope all queries to container to avoid button/text ambiguity from
  // other components in the shared jsdom environment.
  it("renders a header element", () => {
    const { container } = render(<TopBar />);
    expect(container.querySelector("header")).toBeTruthy();
  });

  it("renders the canvas name (default)", () => {
    const { container } = render(<TopBar />);
    expect(container.textContent).toContain("Canvas");
  });

  it("renders a custom canvas name", () => {
    const { container } = render(<TopBar canvasName="My Org Canvas" />);
    expect(container.textContent).toContain("My Org Canvas");
  });

  it("renders the '+ New Agent' button", () => {
    const { container } = render(<TopBar />);
    const btn = Array.from(container.querySelectorAll("button")).find(
      (b) => /new agent/i.test(b.textContent ?? "")
    );
    expect(btn).toBeTruthy();
  });

  it("renders the SettingsButton", () => {
    const { container } = render(<TopBar />);
    const btn = Array.from(container.querySelectorAll("button")).find(
      (b) => b.getAttribute("aria-label") === "Settings"
    );
    expect(btn).toBeTruthy();
  });

  it("has the logo span with aria-hidden", () => {
    const { container } = render(<TopBar />);
    const logo = container.querySelector('[aria-hidden="true"]');
    expect(logo?.textContent).toBe("☁");
  });
});
