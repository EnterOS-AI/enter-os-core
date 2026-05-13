// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react";

// Regression tests for the OrgImportPreflightModal's save path and
// any-of group rendering. Guards two specific bugs caught in the
// UX A/B Lab rollout (2026-04-24):
//
//   1. saveOne early-returned because it tried to read a local
//      `startValue` reassigned inside a functional setDrafts
//      updater. React did not always evaluate the updater
//      synchronously, so the gate read "" and bailed while
//      `saving:true` committed at next render, wedging the
//      button on "…" without ever calling createSecret.
//
//   2. Double-click / Enter-spam could race past the disabled-
//      button UI gate, firing createSecret twice. The production
//      endpoint is idempotent so no data hazard, but the extra
//      PUT is wasteful and harder to reason about.

const { createSecretMock } = vi.hoisted(() => ({
  createSecretMock: vi.fn().mockResolvedValue(undefined),
}));

vi.mock("@/lib/api/secrets", () => ({
  createSecret: (...args: unknown[]) => createSecretMock(...args),
}));

import { OrgImportPreflightModal } from "../OrgImportPreflightModal";

beforeEach(() => {
  createSecretMock.mockClear();
  createSecretMock.mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
});

describe("OrgImportPreflightModal — saveOne", () => {
  it("calls createSecret exactly once when Save is clicked on an any-of member", async () => {
    render(
      <OrgImportPreflightModal
        open
        orgName="UX A/B Lab"
        workspaceCount={7}
        requiredEnv={[{ any_of: ["ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"] }]}
        recommendedEnv={[]}
        configuredKeys={new Set()}
        onSecretSaved={() => {}}
        onProceed={() => {}}
        onCancel={() => {}}
      />,
    );

    // Both any-of members render their own input + Save.
    const input = screen.getByLabelText(/Value for ANTHROPIC_API_KEY/i);
    fireEvent.change(input, { target: { value: "test-secret-value" } });

    // The Save button adjacent to the changed input.
    const saveButtons = screen
      .getAllByRole("button")
      .filter((b) => b.textContent === "Save");
    // Two saves on screen (one per any-of member). First is ANTHROPIC.
    fireEvent.click(saveButtons[0]);

    await waitFor(() => {
      expect(createSecretMock).toHaveBeenCalledTimes(1);
    });
    expect(createSecretMock).toHaveBeenCalledWith(
      "global",
      "ANTHROPIC_API_KEY",
      "test-secret-value",
    );
  });

  it("synchronous double-click on Save fires createSecret exactly once", async () => {
    // Pause the first save so we can fire a second click while the
    // first is still mid-await. The two clicks happen in the SAME
    // tick — fireEvent runs synchronously through React's event
    // system — so any guard that depends on a committed setState
    // (e.g. `disabled={drafts[key].saving}` or a closure read of
    // `drafts[key].saving`) loses the race: the second click sees
    // saving=false because React hasn't committed yet. The fix is
    // a useRef-based gate that flips synchronously before any await.
    let resolveCreate!: () => void;
    createSecretMock.mockImplementationOnce(
      () => new Promise<void>((resolve) => {
        resolveCreate = resolve;
      }),
    );

    render(
      <OrgImportPreflightModal
        open
        orgName="UX A/B Lab"
        workspaceCount={7}
        requiredEnv={[{ any_of: ["ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"] }]}
        recommendedEnv={[]}
        configuredKeys={new Set()}
        onSecretSaved={() => {}}
        onProceed={() => {}}
        onCancel={() => {}}
      />,
    );

    const input = screen.getByLabelText(/Value for ANTHROPIC_API_KEY/i);
    fireEvent.change(input, { target: { value: "test-secret-value" } });

    const saveButtons = screen
      .getAllByRole("button")
      .filter((b) => b.textContent === "Save");
    // Pull the React-bound onClick once so both invocations close
    // over the SAME callback — simulates a double-fire that happens
    // before React reconciles between events. Without this, RTL
    // flushes act() between fireEvent calls and the second click
    // sees the post-commit state.
    const saveBtn = saveButtons[0] as HTMLButtonElement;
    saveBtn.click();
    saveBtn.click();

    // Give React a tick to process any queued state updates.
    await waitFor(() => {
      expect(createSecretMock).toHaveBeenCalledTimes(1);
    });

    resolveCreate();
    await waitFor(() => {
      // Post-save count must remain at exactly one.
      expect(createSecretMock).toHaveBeenCalledTimes(1);
    });
  });

  it("does not call createSecret when value is empty", async () => {
    render(
      <OrgImportPreflightModal
        open
        orgName="UX A/B Lab"
        workspaceCount={7}
        requiredEnv={[{ any_of: ["ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"] }]}
        recommendedEnv={[]}
        configuredKeys={new Set()}
        onSecretSaved={() => {}}
        onProceed={() => {}}
        onCancel={() => {}}
      />,
    );

    // Button is disabled when value is empty — clicking a disabled
    // button still dispatches onClick in RTL (since fireEvent
    // bypasses the disabled attribute), so this asserts the code-
    // level gate catches it, not just the UI.
    const saveButtons = screen
      .getAllByRole("button")
      .filter((b) => b.textContent === "Save");
    fireEvent.click(saveButtons[0]);

    // Small async wait to let any state updates settle.
    await new Promise((r) => setTimeout(r, 50));
    expect(createSecretMock).not.toHaveBeenCalled();
  });
});

describe("OrgImportPreflightModal — any-of rendering", () => {
  it("renders each any-of member as a separate input row", () => {
    render(
      <OrgImportPreflightModal
        open
        orgName="UX A/B Lab"
        workspaceCount={7}
        requiredEnv={[{ any_of: ["ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"] }]}
        recommendedEnv={[]}
        configuredKeys={new Set()}
        onSecretSaved={() => {}}
        onProceed={() => {}}
        onCancel={() => {}}
      />,
    );

    expect(screen.getByText("Configure any one")).toBeTruthy();
    expect(screen.getByLabelText(/Value for ANTHROPIC_API_KEY/i)).toBeTruthy();
    expect(screen.getByLabelText(/Value for CLAUDE_CODE_OAUTH_TOKEN/i)).toBeTruthy();
  });

  it("shows satisfied indicator when any member is configured, and enables Import", () => {
    render(
      <OrgImportPreflightModal
        open
        orgName="UX A/B Lab"
        workspaceCount={7}
        requiredEnv={[{ any_of: ["ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"] }]}
        recommendedEnv={[]}
        configuredKeys={new Set(["CLAUDE_CODE_OAUTH_TOKEN"])}
        onSecretSaved={() => {}}
        onProceed={() => {}}
        onCancel={() => {}}
      />,
    );

    // "✓ using CLAUDE_CODE_OAUTH_TOKEN" banner renders. Name appears
    // twice (banner + member row) so use getAllByText.
    expect(screen.getByText(/using/i)).toBeTruthy();
    expect(screen.getAllByText("CLAUDE_CODE_OAUTH_TOKEN").length).toBeGreaterThanOrEqual(1);

    const importBtn = screen.getByRole("button", { name: /^Import$/ });
    expect(importBtn.hasAttribute("disabled")).toBe(false);
  });

  it("keeps Import disabled when no any-of member is configured", () => {
    render(
      <OrgImportPreflightModal
        open
        orgName="UX A/B Lab"
        workspaceCount={7}
        requiredEnv={[{ any_of: ["ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"] }]}
        recommendedEnv={[]}
        configuredKeys={new Set()}
        onSecretSaved={() => {}}
        onProceed={() => {}}
        onCancel={() => {}}
      />,
    );

    const importBtn = screen.getByRole("button", { name: /^Import$/ });
    expect(importBtn.hasAttribute("disabled")).toBe(true);
  });
});
