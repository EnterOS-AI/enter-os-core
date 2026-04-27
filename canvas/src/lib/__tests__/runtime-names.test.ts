/**
 * Tests for `runtimeDisplayName` — the friendly-name lookup that
 * surfaces the workspace runtime in the chat indicator, details
 * tab, and a few component labels. Tiny but high-touch: every
 * surface that shows "this workspace runs on X" goes through here.
 *
 * Issue: #1815 follow-up — `src/lib/runtime-names.ts` was at 0%
 * coverage despite being read by 3+ rendering paths.
 */
import { describe, it, expect } from "vitest";
import { runtimeDisplayName } from "../runtime-names";

describe("runtimeDisplayName", () => {
  it.each([
    ["claude-code", "Claude Code"],
    ["langgraph", "LangGraph"],
    ["deepagents", "DeepAgents"],
    ["openclaw", "OpenClaw"],
    ["crewai", "CrewAI"],
    ["autogen", "AutoGen"],
  ])("known runtime %q maps to %q", (input, expected) => {
    expect(runtimeDisplayName(input)).toBe(expected);
  });

  it("unknown runtime falls back to the input string verbatim", () => {
    // A future runtime not yet in the lookup map should render with
    // its own id — better than a generic placeholder for ops debugging.
    expect(runtimeDisplayName("hermes")).toBe("hermes");
    expect(runtimeDisplayName("custom-runtime-9000")).toBe(
      "custom-runtime-9000",
    );
  });

  it("empty string falls back to 'agent' (final default)", () => {
    // Any code path that loses the runtime field still renders SOMETHING;
    // the chat indicator never shows a blank label.
    expect(runtimeDisplayName("")).toBe("agent");
  });

  it("is case-sensitive — uppercase variants miss the lookup", () => {
    // The lookup keys are lowercase by convention. Pin the case
    // sensitivity explicitly so a future refactor that lowercases
    // the input "for safety" doesn't silently change behavior — the
    // upstream slug is already normalized lowercase.
    expect(runtimeDisplayName("Claude-Code")).toBe("Claude-Code");
    expect(runtimeDisplayName("LANGGRAPH")).toBe("LANGGRAPH");
  });
});
