// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { ConfirmDialog } from "../ConfirmDialog";

afterEach(() => {
  cleanup();
});

describe("ConfirmDialog — WCAG dialog accessibility", () => {
  it("dialog has role=dialog and aria-modal=true", () => {
    render(
      <ConfirmDialog
        open
        title="Are you sure?"
        message="This action cannot be undone."
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const dialog = screen.getByRole("dialog");
    expect(dialog).toBeTruthy();
    expect(dialog.getAttribute("aria-modal")).toBe("true");
  });

  it("dialog has aria-labelledby pointing to the title", () => {
    render(
      <ConfirmDialog
        open
        title="Delete workspace"
        message="This will permanently delete the workspace."
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const dialog = screen.getByRole("dialog");
    const labelledBy = dialog.getAttribute("aria-labelledby");
    expect(labelledBy).toBeTruthy();
    const titleEl = document.getElementById(labelledBy!);
    expect(titleEl?.textContent?.trim()).toBe("Delete workspace");
  });

  it("Escape key invokes onCancel", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        open
        title="Title"
        message="Message"
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />
    );
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("Enter key invokes onConfirm", () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        open
        title="Title"
        message="Message"
        onConfirm={onConfirm}
        onCancel={vi.fn()}
      />
    );
    fireEvent.keyDown(window, { key: "Enter" });
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("moves focus to the first button when dialog opens (WCAG 2.4.3)", async () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        open
        title="Title"
        message="Message"
        onConfirm={onConfirm}
        onCancel={vi.fn()}
      />
    );
    // Flush requestAnimationFrame so ConfirmDialog's internal rAF focus fires
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    });
    const firstButton = screen.getAllByRole("button")[0];
    expect(document.activeElement).toBe(firstButton);
  });
});

describe("ConfirmDialog — backdrop", () => {
  it("backdrop click invokes onCancel", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        open
        title="Title"
        message="Message"
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />
    );
    const backdrop = document.querySelector('[aria-label="Dismiss dialog"]') as HTMLElement;
    expect(backdrop).toBeTruthy();
    fireEvent.click(backdrop);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});

describe("ConfirmDialog singleButton prop", () => {
  it("renders Cancel button by default", () => {
    render(
      <ConfirmDialog
        open
        title="Title"
        message="Message"
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Confirm" })).toBeTruthy();
  });

  it("hides Cancel button when singleButton=true", () => {
    render(
      <ConfirmDialog
        open
        singleButton
        title="Title"
        message="Message"
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(screen.getByRole("button", { name: "Confirm" })).toBeTruthy();
  });

  it("singleButton: onCancel still fires on Escape", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        open
        singleButton
        title="Title"
        message="Message"
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />
    );
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("singleButton: onCancel still fires on backdrop click", () => {
    const onCancel = vi.fn();
    const { container } = render(
      <ConfirmDialog
        open
        singleButton
        title="Title"
        message="Message"
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />
    );
    // Backdrop is the div with bg-black/60 class, rendered into document.body via portal
    const backdrop = document.querySelector(".bg-black\\/60") as HTMLElement;
    expect(backdrop).toBeTruthy();
    void container;
    fireEvent.click(backdrop);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("backdrop has aria-label for screen reader users (WCAG 4.1.2)", () => {
    render(
      <ConfirmDialog
        open
        title="Title"
        message="Message"
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />
    );
    const backdrop = document.querySelector(".bg-black\\/60");
    expect(backdrop).toBeTruthy();
    expect(backdrop?.getAttribute("aria-label")).toBe("Dismiss dialog");
  });

  it("singleButton: onConfirm fires on button click", () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        open
        singleButton
        title="Title"
        message="Message"
        onConfirm={onConfirm}
        onCancel={vi.fn()}
      />
    );
    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });
});
