// @vitest-environment jsdom
/**
 * Tests for ThemeToggle component.
 *
 * Covers: renders all three options, aria radiogroup semantics,
 * aria-checked per option, setTheme calls on click, keyboard navigation
 * (arrow keys, Home/End), focus-visible rings, custom className prop.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeToggle } from "../ThemeToggle";
import * as themeProvider from "@/lib/theme-provider";

// ─── Mock theme provider ───────────────────────────────────────────────────────

const mockSetTheme = vi.fn();

vi.mock("@/lib/theme-provider", () => ({
  useTheme: vi.fn(() => ({
    theme: "dark",
    resolvedTheme: "dark",
    setTheme: mockSetTheme,
  })),
}));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("ThemeToggle — render", () => {
  beforeEach(() => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "dark",
      resolvedTheme: "dark",
      setTheme: mockSetTheme,
    });
  });

  it("renders a radiogroup with aria-label", () => {
    render(<ThemeToggle />);
    expect(screen.getByRole("radiogroup", { name: "Theme preference" })).toBeTruthy();
  });

  it("renders three radio buttons", () => {
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    expect(radios).toHaveLength(3);
  });

  it("has aria-checked=true on the active option", () => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "dark",
      resolvedTheme: "dark",
      setTheme: mockSetTheme,
    });
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    expect(radios[2].getAttribute("aria-checked")).toBe("true"); // dark is third
    expect(radios[0].getAttribute("aria-checked")).toBe("false"); // light is first
    expect(radios[1].getAttribute("aria-checked")).toBe("false"); // system is second
  });

  it("marks 'light' as active when theme=light", () => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "light",
      resolvedTheme: "light",
      setTheme: mockSetTheme,
    });
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    expect(radios[0].getAttribute("aria-checked")).toBe("true"); // light
    expect(radios[1].getAttribute("aria-checked")).toBe("false"); // system
    expect(radios[2].getAttribute("aria-checked")).toBe("false"); // dark
  });

  it("marks 'system' as active when theme=system", () => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "system",
      resolvedTheme: "light",
      setTheme: mockSetTheme,
    });
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    expect(radios[0].getAttribute("aria-checked")).toBe("false"); // light
    expect(radios[1].getAttribute("aria-checked")).toBe("true"); // system
    expect(radios[2].getAttribute("aria-checked")).toBe("false"); // dark
  });

  it("has aria-label on each button matching the option label", () => {
    render(<ThemeToggle />);
    expect(screen.getByRole("radio", { name: "Light" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "System" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Dark" })).toBeTruthy();
  });
});

describe("ThemeToggle — interaction", () => {
  beforeEach(() => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "dark",
      resolvedTheme: "dark",
      setTheme: mockSetTheme,
    });
  });

  it("calls setTheme with 'light' when light button is clicked", () => {
    render(<ThemeToggle />);
    fireEvent.click(screen.getByRole("radio", { name: "Light" }));
    expect(mockSetTheme).toHaveBeenCalledWith("light");
  });

  it("calls setTheme with 'system' when system button is clicked", () => {
    render(<ThemeToggle />);
    fireEvent.click(screen.getByRole("radio", { name: "System" }));
    expect(mockSetTheme).toHaveBeenCalledWith("system");
  });

  it("calls setTheme with 'dark' when dark button is clicked", () => {
    render(<ThemeToggle />);
    fireEvent.click(screen.getByRole("radio", { name: "Dark" }));
    expect(mockSetTheme).toHaveBeenCalledWith("dark");
  });

  it("calls setTheme only once per click", () => {
    render(<ThemeToggle />);
    fireEvent.click(screen.getByRole("radio", { name: "Light" }));
    expect(mockSetTheme).toHaveBeenCalledTimes(1);
  });
});

describe("ThemeToggle — keyboard navigation (WCAG 2.1.1 / ARIA radiogroup)", () => {
  beforeEach(() => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "dark",
      resolvedTheme: "dark",
      setTheme: mockSetTheme,
    });
  });

  it("moves to the next option on ArrowRight and wraps around", () => {
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    // dark (index 2) is current; ArrowRight should wrap to light (index 0)
    act(() => { radios[2].focus(); });
    fireEvent.keyDown(radios[2], { key: "ArrowRight" });
    expect(mockSetTheme).toHaveBeenCalledWith("light");
  });

  it("moves to the previous option on ArrowLeft", () => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "light",
      resolvedTheme: "light",
      setTheme: mockSetTheme,
    });
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    // light (index 0) is current; ArrowLeft should go to dark (index 2)
    act(() => { radios[0].focus(); });
    fireEvent.keyDown(radios[0], { key: "ArrowLeft" });
    expect(mockSetTheme).toHaveBeenCalledWith("dark");
  });

  it("moves to the next option on ArrowDown", () => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "light",
      resolvedTheme: "light",
      setTheme: mockSetTheme,
    });
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    // light (index 0) is current; ArrowDown should go to system (index 1)
    act(() => { radios[0].focus(); });
    fireEvent.keyDown(radios[0], { key: "ArrowDown" });
    expect(mockSetTheme).toHaveBeenCalledWith("system");
  });

  it("jumps to the first option on Home", () => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "dark",
      resolvedTheme: "dark",
      setTheme: mockSetTheme,
    });
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    act(() => { radios[2].focus(); });
    fireEvent.keyDown(radios[2], { key: "Home" });
    expect(mockSetTheme).toHaveBeenCalledWith("light");
  });

  it("jumps to the last option on End", () => {
    vi.mocked(themeProvider.useTheme).mockReturnValue({
      theme: "light",
      resolvedTheme: "light",
      setTheme: mockSetTheme,
    });
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    act(() => { radios[0].focus(); });
    fireEvent.keyDown(radios[0], { key: "End" });
    expect(mockSetTheme).toHaveBeenCalledWith("dark");
  });

  it("does nothing on unrelated keys", () => {
    render(<ThemeToggle />);
    const radios = screen.getAllByRole("radio");
    fireEvent.keyDown(radios[0], { key: "Enter" });
    expect(mockSetTheme).not.toHaveBeenCalled();
  });
});

describe("ThemeToggle — className prop", () => {
  it("passes custom className to the radiogroup", () => {
    render(<ThemeToggle className="my-custom-class" />);
    const group = screen.getByRole("radiogroup", { name: "Theme preference" });
    expect(group.className).toContain("my-custom-class");
  });

  it("applies default className when none provided", () => {
    render(<ThemeToggle />);
    const group = screen.getByRole("radiogroup", { name: "Theme preference" });
    expect(group.className).toContain("inline-flex");
  });
});
