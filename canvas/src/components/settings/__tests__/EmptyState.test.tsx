// @vitest-environment jsdom
/**
 * Settings EmptyState — shown when no secrets exist.
 *
 * Per spec §3.2:
 *   🔑
 *   No API keys yet
 *   Add your API keys to let agents connect
 *   to GitHub, Anthropic, OpenRouter, and more.
 *   [+ Add your first API key]
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs.
 *
 * Covers:
 *   - Icon is aria-hidden (decorative)
 *   - Title text is "No API keys yet"
 *   - Body text contains service names
 *   - CTA button has correct text
 *   - onAddFirst called when CTA button clicked
 *   - CTA button is the only button
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { EmptyState } from "../EmptyState";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── Render ────────────────────────────────────────────────────────────────────

describe("Settings EmptyState — render", () => {
  it("icon is aria-hidden", () => {
    const { container } = render(
      <EmptyState onAddFirst={vi.fn()} />,
    );
    const icon = container.querySelector('[aria-hidden="true"]');
    expect(icon).toBeTruthy();
    expect(icon?.textContent).toContain("🔑");
  });

  it("title text is 'No API keys yet'", () => {
    render(<EmptyState onAddFirst={vi.fn()} />);
    expect(document.body.textContent).toContain("No API keys yet");
  });

  it("body text contains service names", () => {
    render(<EmptyState onAddFirst={vi.fn()} />);
    const text = document.body.textContent ?? "";
    expect(text).toContain("GitHub");
    expect(text).toContain("Anthropic");
    expect(text).toContain("OpenRouter");
  });

  it("CTA button has correct text", () => {
    render(<EmptyState onAddFirst={vi.fn()} />);
    const btn = document.querySelector("button");
    expect(btn?.textContent).toContain("Add your first API key");
  });

  it("CTA button is the only button in the component", () => {
    const { container } = render(
      <EmptyState onAddFirst={vi.fn()} />,
    );
    expect(container.querySelectorAll("button")).toHaveLength(1);
  });
});

// ─── Interaction ───────────────────────────────────────────────────────────────

describe("Settings EmptyState — interaction", () => {
  it("onAddFirst called when CTA button clicked", () => {
    const onAddFirst = vi.fn();
    render(<EmptyState onAddFirst={onAddFirst} />);
    const btn = document.querySelector("button") as HTMLButtonElement;
    btn.click();
    expect(onAddFirst).toHaveBeenCalledTimes(1);
  });
});
