// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { ConfirmDialog } from "../ConfirmDialog";

afterEach(() => {
  cleanup();
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
