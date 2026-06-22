// @vitest-environment jsdom
/**
 * Tests for the Z keyboard shortcut (zoom-to-team) and help panel entry.
 */
import React from "react";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// vi.mock is hoisted to module top level by Vitest regardless of where it appears
// in the source. Placing it here explicitly matches that runtime behaviour and
// silences the "not at top level" warning (closes #632).
vi.mock("../../../store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn(() => null),
    {
      getState: () => ({
        selectedNodeId: null,
        nodes: [],
        contextMenu: null,
        closeContextMenu: vi.fn(),
        selectNode: vi.fn(),
      }),
    }
  ),
}));

afterEach(() => cleanup());

// ─── Z key handler unit tests (no React needed) ─────────────────────────────

describe("Z key → molecule:zoom-to-team", () => {
  let dispatchedEvents: CustomEvent[] = [];

  beforeEach(() => {
    dispatchedEvents = [];
    window.addEventListener("molecule:zoom-to-team", (e) => {
      dispatchedEvents.push(e as CustomEvent);
    });
  });

  afterEach(() => {
    window.removeEventListener("molecule:zoom-to-team", () => {});
  });

  it("does NOT fire when no node is selected", () => {
    fireEvent.keyDown(window, { key: "Z" });
    expect(dispatchedEvents).toHaveLength(0);
  });

  it("does NOT fire when target is an input element", () => {
    const input = document.createElement("input");
    document.body.appendChild(input);
    fireEvent.keyDown(input, { key: "Z" });
    expect(dispatchedEvents).toHaveLength(0);
    document.body.removeChild(input);
  });
});

// ─── Help panel text test ────────────────────────────────────────────────────

describe("Toolbar help panel — zoom shortcut entry", () => {
  it("help panel content mentions double-click / Z gesture", async () => {
    // Read the source to verify the entry is present (static assertion)
    const { readFileSync } = await import("fs");
    const { join } = await import("path");
    const src = readFileSync(
      join(__dirname, "../../components/Toolbar.tsx"),
      "utf8"
    );
    expect(src).toContain("Dbl-click");
    expect(src).toContain("Zoom canvas to fit a team node");
  });

  it("Keyboard shortcuts hook guards against input elements", async () => {
    const { readFileSync } = await import("fs");
    const { join } = await import("path");
    // After the canvas split (commit c5abed98 → f3423a51 series), the
    // Z-key / hierarchy / zoom shortcuts moved out of Canvas.tsx into
    // the useKeyboardShortcuts hook under src/components/canvas/.
    const src = readFileSync(
      join(__dirname, "../../components/canvas/useKeyboardShortcuts.ts"),
      "utf8"
    );
    expect(src).toContain('e.key === "z" || e.key === "Z"');
    expect(src).toContain("molecule:zoom-to-team");
    expect(src).toContain('tag === "INPUT"');
  });
});
