// @vitest-environment jsdom
/**
 * Tests for readThemeCookie — parses a cookie value into a ThemePreference.
 */
import { describe, it, expect } from "vitest";
import { readThemeCookie } from "../theme-cookie";

describe("readThemeCookie", () => {
  it('returns "light" when cookie value is "light"', () => {
    expect(readThemeCookie("light")).toBe("light");
  });

  it('returns "dark" when cookie value is "dark"', () => {
    expect(readThemeCookie("dark")).toBe("dark");
  });

  it('returns "system" when cookie value is "system"', () => {
    expect(readThemeCookie("system")).toBe("system");
  });

  it('returns "system" for undefined', () => {
    expect(readThemeCookie(undefined)).toBe("system");
  });

  it('returns "system" for empty string', () => {
    expect(readThemeCookie("")).toBe("system");
  });

  it('returns "system" for any non-matching value', () => {
    expect(readThemeCookie("auto")).toBe("system");
    expect(readThemeCookie("dark-mode")).toBe("system");
    expect(readThemeCookie("DARK")).toBe("system"); // case-sensitive
    expect(readThemeCookie("light\n")).toBe("system"); // whitespace-sensitive
    expect(readThemeCookie("  system  ")).toBe("system");
    expect(readThemeCookie("null")).toBe("system");
    expect(readThemeCookie("0")).toBe("system");
  });

  it("is pure — same input always returns same output", () => {
    const inputs = ["light", "dark", "system", undefined, ""];
    for (const input of inputs) {
      for (let i = 0; i < 3; i++) {
        expect(readThemeCookie(input)).toBe(readThemeCookie(input));
      }
    }
  });
});
