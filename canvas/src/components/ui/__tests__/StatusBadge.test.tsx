// @vitest-environment jsdom
/**
 * StatusBadge — secret key connection status indicator.
 *
 * Per spec §4: always icon + color (never colour-only) for colour-blind users.
 * Covers: verified / invalid / unverified render branches, icon, aria-label, className.
 */
import { afterEach, describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import React from "react";

import { StatusBadge } from "../StatusBadge";

afterEach(() => {
  // Prevent DOM accumulation across tests (maxWorkers=1 means all test
  // files share the same jsdom worker).
  const { cleanup } = require("@testing-library/react");
  cleanup();
});

function getBadge(status: "verified" | "invalid" | "unverified") {
  const { container } = render(<StatusBadge status={status} />);
  return container.querySelector("[role=status]") as HTMLElement;
}

describe("StatusBadge — icon", () => {
  it("renders ✓ for verified", () => {
    expect(getBadge("verified").textContent).toBe("✓");
  });

  it("renders ✗ for invalid", () => {
    expect(getBadge("invalid").textContent).toBe("✗");
  });

  it("renders ○ for unverified", () => {
    expect(getBadge("unverified").textContent).toBe("○");
  });
});

describe("StatusBadge — aria-label", () => {
  it("sets 'Connection status: verified' for verified", () => {
    expect(getBadge("verified").getAttribute("aria-label")).toBe(
      "Connection status: verified",
    );
  });

  it("sets 'Connection status: invalid' for invalid", () => {
    expect(getBadge("invalid").getAttribute("aria-label")).toBe(
      "Connection status: invalid",
    );
  });

  it("sets 'Connection status: unverified' for unverified", () => {
    expect(getBadge("unverified").getAttribute("aria-label")).toBe(
      "Connection status: unverified",
    );
  });
});

describe("StatusBadge — className", () => {
  it("applies status-badge--valid for verified", () => {
    expect(getBadge("verified").className).toContain("status-badge--valid");
  });

  it("applies status-badge--invalid for invalid", () => {
    expect(getBadge("invalid").className).toContain("status-badge--invalid");
  });

  it("applies status-badge--unverified for unverified", () => {
    expect(getBadge("unverified").className).toContain(
      "status-badge--unverified",
    );
  });
});

describe("StatusBadge — role", () => {
  it("sets role=status", () => {
    const el = getBadge("verified");
    expect(el.getAttribute("role")).toBe("status");
  });
});

describe("StatusBadge — structural", () => {
  it("renders exactly one status element", () => {
    const { container } = render(<StatusBadge status="verified" />);
    expect(container.querySelectorAll("[role=status]").length).toBe(1);
  });
});
