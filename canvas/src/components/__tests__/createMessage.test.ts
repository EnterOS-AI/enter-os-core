// @vitest-environment jsdom
/**
 * Tests for createMessage — the ChatMessage factory from types.ts.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { createMessage } from "../tabs/chat/types";

describe("createMessage", () => {
  beforeEach(() => {
    // Freeze time so timestamp is deterministic.
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-05-10T12:00:00.000Z"));
    // Stub crypto.randomUUID so message IDs are deterministic.
    vi.stubGlobal("crypto", { randomUUID: vi.fn(() => "fixed-uuid-1234") });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("creates a message with the correct role", () => {
    const userMsg = createMessage("user", "hello");
    expect(userMsg.role).toBe("user");

    const agentMsg = createMessage("agent", "hi there");
    expect(agentMsg.role).toBe("agent");

    const systemMsg = createMessage("system", "prompt loaded");
    expect(systemMsg.role).toBe("system");
  });

  it("creates a message with the correct content", () => {
    const msg = createMessage("user", "Deploy the agent now");
    expect(msg.content).toBe("Deploy the agent now");
  });

  it("sets a deterministic id via crypto.randomUUID", () => {
    const msg = createMessage("agent", "response");
    expect(msg.id).toBe("fixed-uuid-1234");
  });

  it("sets a deterministic ISO timestamp", () => {
    const msg = createMessage("user", "hello");
    expect(msg.timestamp).toBe("2026-05-10T12:00:00.000Z");
  });

  it("omits attachments field when none provided", () => {
    const msg = createMessage("user", "hello");
    expect(msg.attachments).toBeUndefined();
  });

  it("omits attachments field when empty array is provided", () => {
    const msg = createMessage("agent", "result", []);
    expect(msg.attachments).toBeUndefined();
  });

  it("includes attachments field when non-empty array is provided", () => {
    const atts = [{ name: "report.pdf", uri: "workspace:/docs/report.pdf" }];
    const msg = createMessage("agent", "see attached", atts);
    expect(msg.attachments).toEqual(atts);
  });

  it("returns a frozen object (prevents accidental mutation)", () => {
    const msg = createMessage("user", "hello");
    expect(Object.isFrozen(msg)).toBe(true);
  });

  it("returns a plain object with expected keys", () => {
    const msg = createMessage("user", "hello");
    expect(Object.keys(msg).sort()).toEqual(
      ["id", "role", "content", "timestamp"].sort()
    );
  });
});
