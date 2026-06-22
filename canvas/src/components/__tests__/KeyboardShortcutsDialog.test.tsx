// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";

// ── Component under test — imported AFTER mocks ───────────────────────────────
import { KeyboardShortcutsDialog } from "../KeyboardShortcutsDialog";

afterEach(cleanup);

const onCloseMock = vi.fn();

beforeEach(() => {
  onCloseMock.mockReset();
});

describe("KeyboardShortcutsDialog — a11y render", () => {
  it("renders with role=dialog and aria-modal=true when open", async () => {
    render(<KeyboardShortcutsDialog open={true} onClose={onCloseMock} />);
    await waitFor(() => {
      expect(screen.getByRole("dialog")).toBeTruthy();
    });
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("aria-modal")).toBe("true");
  });

  it("has aria-labelledby pointing to the dialog title", async () => {
    render(<KeyboardShortcutsDialog open={true} onClose={onCloseMock} />);
    const dialog = await waitFor(() => screen.getByRole("dialog"));
    const labelledby = dialog.getAttribute("aria-labelledby");
    expect(labelledby).toBeTruthy();
    // The labelledby should reference the h2 with id="keyboard-shortcuts-title"
    const title = document.getElementById(labelledby!);
    expect(title?.textContent).toMatch(/keyboard shortcuts/i);
  });

  it("does not render when open=false", () => {
    render(<KeyboardShortcutsDialog open={false} onClose={onCloseMock} />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("calls onClose when Escape is pressed", async () => {
    render(<KeyboardShortcutsDialog open={true} onClose={onCloseMock} />);
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    expect(onCloseMock).toHaveBeenCalledTimes(1);
  });

  it("focuses the first focusable element (close button) when dialog opens", async () => {
    render(<KeyboardShortcutsDialog open={true} onClose={onCloseMock} />);
    // The component uses requestAnimationFrame to move focus; wait for it to settle.
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    });
    const closeBtn = screen.getByRole("button", { name: /close/i });
    expect(document.activeElement).toBe(closeBtn);
  });

  it("traps Tab focus within the dialog", async () => {
    render(<KeyboardShortcutsDialog open={true} onClose={onCloseMock} />);
    const dialog = await waitFor(() => screen.getByRole("dialog"));

    // Collect all focusable elements inside the dialog
    const focusableSelectors =
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    const focusableEls = Array.from(
      dialog.querySelectorAll<HTMLElement>(focusableSelectors)
    );
    expect(focusableEls.length).toBeGreaterThan(0);

    const onlyFocusable = focusableEls[0];
    act(() => { onlyFocusable.focus(); });

    // Simulate Tab keydown. The dialog's handler should call preventDefault()
    // to stop focus leaving the dialog. Verify by checking the event was
    // handled (focus remains on the only focusable element).
    let tabWasIntercepted = false;
    const tabHandler = (e: KeyboardEvent) => {
      if (e.key === "Tab") tabWasIntercepted = e.defaultPrevented;
    };
    window.addEventListener("keydown", tabHandler);
    act(() => {
      fireEvent.keyDown(onlyFocusable, { key: "Tab", shiftKey: false });
    });
    expect(tabWasIntercepted).toBe(true);
    window.removeEventListener("keydown", tabHandler);
  });
});
