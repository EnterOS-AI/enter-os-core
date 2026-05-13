// @vitest-environment jsdom
/**
 * Tests for NotAvailablePanel — the full-tab placeholder shown when a
 * workspace's runtime doesn't own a platform-managed filesystem (today:
 * runtime === "external"). Covers rendering, a11y, and runtime prop
 * display.
 */
import React from "react";
import { render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { NotAvailablePanel } from "../NotAvailablePanel";

afterEach(cleanup);

describe("NotAvailablePanel", () => {
  describe("renders", () => {
    it("renders the heading", () => {
      render(<NotAvailablePanel runtime="external" />);
      expect(screen.getByText("Files not available")).toBeTruthy();
    });

    it("renders the description text", () => {
      render(<NotAvailablePanel runtime="external" />);
      expect(
        screen.getByText(/whose filesystem isn't owned by the platform/i)
      ).toBeTruthy();
    });

    it("displays the runtime name in the description", () => {
      render(<NotAvailablePanel runtime="aws-lambda" />);
      // The runtime name appears inside the paragraph
      const para = screen.getByText(/whose filesystem isn't owned/i);
      expect(para.textContent).toContain("aws-lambda");
    });

    it("renders the SVG folder icon with aria-hidden", () => {
      render(<NotAvailablePanel runtime="external" />);
      const svg = document.querySelector("svg");
      expect(svg).toBeTruthy();
      expect(svg?.getAttribute("aria-hidden")).toBe("true");
    });

    it("uses the provided runtime prop verbatim", () => {
      render(<NotAvailablePanel runtime="cloud-run" />);
      const monoRuntime = document.querySelector(".font-mono");
      expect(monoRuntime?.textContent).toBe("cloud-run");
    });

    it("renders the 'Use the Chat tab' guidance text", () => {
      render(<NotAvailablePanel runtime="external" />);
      expect(screen.getByText(/Use the Chat tab/i)).toBeTruthy();
    });

    it("is contained in a full-height flex column", () => {
      render(<NotAvailablePanel runtime="external" />);
      const container = screen.getByText("Files not available").closest("div");
      expect(container?.className).toContain("flex");
      expect(container?.className).toContain("flex-col");
      expect(container?.className).toContain("items-center");
      expect(container?.className).toContain("justify-center");
      expect(container?.className).toContain("h-full");
    });
  });

  describe("a11y", () => {
    it("heading is an h3", () => {
      render(<NotAvailablePanel runtime="external" />);
      expect(screen.getByRole("heading", { level: 3 })).toBeTruthy();
    });

    it("SVG icon has aria-hidden so screen readers skip it", () => {
      render(<NotAvailablePanel runtime="external" />);
      const svg = document.querySelector("svg");
      expect(svg?.getAttribute("aria-hidden")).toBe("true");
    });

    it("description paragraph is present with descriptive text", () => {
      render(<NotAvailablePanel runtime="external" />);
      const paras = document.querySelectorAll("p");
      expect(paras.length).toBeGreaterThan(0);
      const text = Array.from(paras)
        .map((p) => p.textContent)
        .join(" ");
      expect(text.toLowerCase()).toContain("runtime");
    });
  });

  describe("props", () => {
    it("renders with a short runtime name", () => {
      render(<NotAvailablePanel runtime="ext" />);
      const monoRuntime = document.querySelector(".font-mono");
      expect(monoRuntime?.textContent).toBe("ext");
    });

    it("renders with a complex runtime name", () => {
      render(<NotAvailablePanel runtime="gcp-cloud-functions-v2" />);
      const monoRuntime = document.querySelector(".font-mono");
      expect(monoRuntime?.textContent).toBe("gcp-cloud-functions-v2");
    });
  });
});
