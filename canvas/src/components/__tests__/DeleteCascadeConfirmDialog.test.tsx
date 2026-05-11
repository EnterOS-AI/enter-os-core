// @vitest-environment jsdom
/**
 * DeleteCascadeConfirmDialog — WCAG 2.1 dialog accessibility + interaction tests
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react";

afterEach(cleanup);

import { DeleteCascadeConfirmDialog } from "../DeleteCascadeConfirmDialog";

const defaultProps = {
  name: "Test Workspace",
  children: [
    { id: "ws-child-1", name: "Child Workspace 1" },
    { id: "ws-child-2", name: "Child Workspace 2" },
  ],
  checked: false,
  onCheckedChange: vi.fn(),
  onConfirm: vi.fn(),
  onCancel: vi.fn(),
};

function renderDialog(props = {}) {
  return render(<DeleteCascadeConfirmDialog {...defaultProps} {...props} />);
}

describe("DeleteCascadeConfirmDialog — basic rendering", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders the dialog with correct title", () => {
    renderDialog();
    expect(screen.getByText("Delete Workspace and Children")).toBeTruthy();
  });

  it("renders child workspace names in the list", () => {
    renderDialog();
    expect(screen.getByText("Child Workspace 1")).toBeTruthy();
    expect(screen.getByText("Child Workspace 2")).toBeTruthy();
  });

  it("Delete All button is disabled when checkbox is unchecked", () => {
    renderDialog({ checked: false });
    const deleteBtn = screen.getByRole("button", { name: "Delete All" });
    // disabled={!checked}={!false}={true} → button has disabled attribute
    expect(deleteBtn.getAttribute("disabled") !== null).toBe(true);
  });

  it("Delete All button is enabled when checkbox is checked", () => {
    renderDialog({ checked: true });
    const deleteBtn = screen.getByRole("button", { name: "Delete All" });
    expect(deleteBtn.getAttribute("disabled")).toBeFalsy();
  });

  it("checking the checkbox calls onCheckedChange", () => {
    renderDialog();
    const checkbox = screen.getByRole("checkbox");
    fireEvent.click(checkbox);
    expect(defaultProps.onCheckedChange).toHaveBeenCalledWith(true);
  });

  it("Cancel button calls onCancel", () => {
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(defaultProps.onCancel).toHaveBeenCalledTimes(1);
  });

  it("Delete All button calls onConfirm when enabled", () => {
    renderDialog({ checked: true });
    fireEvent.click(screen.getByRole("button", { name: "Delete All" }));
    expect(defaultProps.onConfirm).toHaveBeenCalledTimes(1);
  });
});

describe("DeleteCascadeConfirmDialog — WCAG 2.1 dialog accessibility", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders role=dialog", () => {
    renderDialog();
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("dialog has aria-modal='true' (WCAG 2.1 SC 1.3.2)", () => {
    renderDialog();
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("aria-modal")).toBe("true");
  });

  it("dialog has aria-labelledby pointing to the title", () => {
    renderDialog();
    const dialog = screen.getByRole("dialog");
    const labelledBy = dialog.getAttribute("aria-labelledby");
    expect(labelledBy).toBeTruthy();
    const titleEl = document.getElementById(labelledBy!);
    expect(titleEl?.textContent?.trim()).toBe("Delete Workspace and Children");
  });

  it("backdrop div has aria-label for screen readers (WCAG 2.4.6)", () => {
    renderDialog();
    const backdrop = document.querySelector('[aria-label="Dismiss dialog"]');
    expect(backdrop).toBeTruthy();
    expect(backdrop?.className).toContain("bg-black");
  });

  it("warning SVG icon has aria-hidden='true' (decorative)", () => {
    renderDialog();
    const dialog = screen.getByRole("dialog");
    const svgIcons = dialog.querySelectorAll("svg");
    // The warning triangle SVG should have aria-hidden
    const warningSvg = svgIcons[0];
    expect(warningSvg?.getAttribute("aria-hidden")).toBe("true");
  });

  it("all interactive buttons have accessible names", () => {
    renderDialog();
    const buttons = screen.getAllByRole("button");
    for (const btn of buttons) {
      const name = btn.textContent?.trim();
      expect(name?.length).toBeGreaterThan(0);
    }
  });

  it("checkbox is labelled by the cascade warning text", () => {
    renderDialog();
    const checkbox = screen.getByRole("checkbox");
    expect(checkbox).toBeTruthy();
    // The label wrapping the checkbox provides the accessible name
    expect(
      screen.getByText(/I understand this will permanently delete/i),
    ).toBeTruthy();
  });
});

describe("DeleteCascadeConfirmDialog — keyboard interaction", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("Escape key calls onCancel", () => {
    renderDialog();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(defaultProps.onCancel).toHaveBeenCalledTimes(1);
  });

  it("Enter key on checkbox does NOT confirm when unchecked", () => {
    renderDialog({ checked: false });
    const checkbox = screen.getByRole("checkbox");
    checkbox.focus();
    fireEvent.keyDown(checkbox, { key: "Enter" });
    // onConfirm should NOT be called because checkbox is unchecked
    expect(defaultProps.onConfirm).not.toHaveBeenCalled();
  });

  it("Enter key on checkbox confirms when checked", () => {
    renderDialog({ checked: true });
    const checkbox = screen.getByRole("checkbox");
    checkbox.focus();
    fireEvent.keyDown(checkbox, { key: "Enter" });
    expect(defaultProps.onConfirm).toHaveBeenCalledTimes(1);
  });
});
