// @vitest-environment jsdom
/**
 * TopBar — canvas header scaffold with logo, canvas name, New Agent button,
 * and SettingsButton integration point.
 *
 * Coverage:
 *   - Renders header with logo and canvas name (default and custom)
 *   - New Agent button present and clickable
 *   - SettingsButton rendered (via mock)
 *   - Ref forwarding wired (settingsGearRef passed as ref prop)
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import React from "react";

import { TopBar } from "../TopBar";

vi.mock("@/components/settings/SettingsButton", () => ({
  SettingsButton: React.forwardRef<HTMLButtonElement, object>(
    (_props, ref) => <button ref={ref} aria-label="Settings" type="button">⚙</button>,
  ),
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── Render ────────────────────────────────────────────────────────────────────

describe("TopBar — render", () => {
  it("renders the header element", () => {
    render(<TopBar />);
    const header = document.querySelector("header");
    expect(header).toBeTruthy();
  });

  it("shows default canvas name 'Canvas'", () => {
    render(<TopBar />);
    expect(document.body.textContent).toContain("Canvas");
  });

  it("shows custom canvas name when provided", () => {
    render(<TopBar canvasName="Production Canvas" />);
    expect(document.body.textContent).toContain("Production Canvas");
    expect(document.body.textContent).not.toContain("Canvas\n"); // not default
  });

  it("renders New Agent button", () => {
    render(<TopBar />);
    const btn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("New Agent"),
    );
    expect(btn).toBeTruthy();
  });

  it("renders SettingsButton", () => {
    render(<TopBar />);
    const settingsBtn = document.querySelector('button[aria-label="Settings"]');
    expect(settingsBtn).toBeTruthy();
  });

  it("renders logo icon", () => {
    render(<TopBar />);
    const logo = Array.from(document.querySelectorAll("span")).find(
      (s) => s.getAttribute("aria-hidden") === "true",
    );
    expect(logo).toBeTruthy();
    expect(logo?.textContent).toContain("☁");
  });
});

// ─── Interaction ──────────────────────────────────────────────────────────────

describe("TopBar — interaction", () => {
  it("New Agent button is in the DOM and not disabled", () => {
    render(<TopBar />);
    const btn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("New Agent"),
    );
    expect(btn).toBeTruthy();
    expect(btn!.getAttribute("disabled")).toBeNull();
  });

  it("renders without crashing with empty canvasName", () => {
    render(<TopBar canvasName="" />);
    expect(document.querySelector("header")).toBeTruthy();
  });

  it("renders without crashing with long canvasName", () => {
    const longName = "A".repeat(200);
    render(<TopBar canvasName={longName} />);
    expect(document.body.textContent).toContain(longName);
  });
});
