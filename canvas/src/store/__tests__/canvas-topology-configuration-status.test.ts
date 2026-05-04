import { describe, it, expect } from "vitest";
import {
  getConfigurationStatus,
  getConfigurationError,
} from "../canvas-topology";

// Tests for the getConfigurationStatus / getConfigurationError helpers
// (issue #467 / PR #2756 chain). Surfacing the workspace's
// `agent_card.configuration_status` is the user-visible payoff of
// PR #2756's decoupling — without it, a misconfigured workspace looks
// identical to a healthy one in the canvas tile.

describe("getConfigurationStatus", () => {
  it("returns null when agentCard is null", () => {
    expect(getConfigurationStatus(null)).toBe(null);
  });

  it("returns null when agentCard has no configuration_status", () => {
    expect(getConfigurationStatus({ name: "x" })).toBe(null);
  });

  it("returns 'ready' when agent reports configuration ok", () => {
    expect(
      getConfigurationStatus({ configuration_status: "ready" }),
    ).toBe("ready");
  });

  it("returns 'not_configured' when agent reports setup failed", () => {
    expect(
      getConfigurationStatus({ configuration_status: "not_configured" }),
    ).toBe("not_configured");
  });

  it("ignores unknown values defensively", () => {
    // A future agent reporting a status string we don't yet recognise
    // shouldn't crash the canvas — we treat it as 'no info' (null).
    expect(
      getConfigurationStatus({ configuration_status: "starting" }),
    ).toBe(null);
    expect(
      getConfigurationStatus({ configuration_status: 42 }),
    ).toBe(null);
    expect(
      getConfigurationStatus({ configuration_status: null }),
    ).toBe(null);
  });
});

describe("getConfigurationError", () => {
  it("returns null when agentCard is null", () => {
    expect(getConfigurationError(null)).toBe(null);
  });

  it("returns null when status is 'ready' even if error string present", () => {
    // Defensive: if the agent somehow ships configuration_status=ready
    // alongside a stale configuration_error from a previous boot, we
    // trust the live status flag and don't surface the stale error.
    expect(
      getConfigurationError({
        configuration_status: "ready",
        configuration_error: "stale: was unset",
      }),
    ).toBe(null);
  });

  it("returns the error string when status is 'not_configured'", () => {
    expect(
      getConfigurationError({
        configuration_status: "not_configured",
        configuration_error:
          "RuntimeError: Neither OPENAI_API_KEY nor MINIMAX_API_KEY is set",
      }),
    ).toBe(
      "RuntimeError: Neither OPENAI_API_KEY nor MINIMAX_API_KEY is set",
    );
  });

  it("returns null when status is 'not_configured' but error is missing", () => {
    expect(
      getConfigurationError({ configuration_status: "not_configured" }),
    ).toBe(null);
  });

  it("returns null when error is empty string", () => {
    // Empty string isn't actionable for the operator — treat same as
    // missing.
    expect(
      getConfigurationError({
        configuration_status: "not_configured",
        configuration_error: "",
      }),
    ).toBe(null);
  });

  it("returns null when error is non-string", () => {
    expect(
      getConfigurationError({
        configuration_status: "not_configured",
        configuration_error: { reason: "object" },
      }),
    ).toBe(null);
  });
});
