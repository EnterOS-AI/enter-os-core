// @vitest-environment jsdom
/**
 * Tests for TopBar component.
 *
 * Covers: renders header, logo, canvas name, "+ New Agent" button,
 * SettingsButton integration, custom canvasName prop.
 */
import React from "react";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { TopBar } from "../canvas/TopBar";

// ─── Mock SettingsButton ───────────────────────────────────────────────────────

vi.mock("../settings/SettingsButton", () => ({
  SettingsButton: vi.fn(() => <button aria-label="Settings">⚙</button>),
}));

describe("TopBar — render", () => {
  it("renders a header element", () => {
    render(<TopBar />);
    expect(document.body.querySelector("header")).toBeTruthy();
  });

  it("renders the canvas name (default)", () => {
    render(<TopBar />);
    expect(screen.getByText("Canvas")).toBeTruthy();
  });

  it("renders a custom canvas name", () => {
    render(<TopBar canvasName="My Org Canvas" />);
    expect(screen.getByText("My Org Canvas")).toBeTruthy();
  });

  it("renders the '+ New Agent' button", () => {
    render(<TopBar />);
    expect(screen.getByRole("button", { name: /new agent/i })).toBeTruthy();
  });

  it("renders the SettingsButton", () => {
    render(<TopBar />);
    expect(screen.getByRole("button", { name: "Settings" })).toBeTruthy();
  });

  it("has the logo span with aria-hidden", () => {
    render(<TopBar />);
    const logo = document.body.querySelector('[aria-hidden="true"]');
    expect(logo?.textContent).toBe("☁");
  });
});
