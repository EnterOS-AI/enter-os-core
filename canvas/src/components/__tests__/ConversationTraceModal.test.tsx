// @vitest-environment jsdom
/**
 * Tests for ConversationTraceModal's extractMessageText helper.
 *
 * Covers: MCP simple task format, request params.message.parts extraction,
 * response result.parts extraction, result.root.text extraction, plain string
 * result, null input, malformed input, empty strings.
 */
import { describe, expect, it } from "vitest";
import { extractMessageText } from "../ConversationTraceModal";

describe("extractMessageText — MCP simple task format", () => {
  it("extracts text from body.task field", () => {
    const body = { task: "Deploy the agent to production" };
    expect(extractMessageText(body)).toBe("Deploy the agent to production");
  });

  it("returns empty string when body is null", () => {
    expect(extractMessageText(null)).toBe("");
  });

  it("returns empty string when body is undefined", () => {
    expect(extractMessageText(undefined as unknown as null)).toBe("");
  });
});

describe("extractMessageText — request params.message format", () => {
  it("extracts text from params.message.parts[].text", () => {
    const body = {
      params: {
        message: {
          parts: [{ text: "Hello world" }],
        },
      },
    };
    expect(extractMessageText(body)).toBe("Hello world");
  });

  it("joins multiple parts with newlines", () => {
    const body = {
      params: {
        message: {
          parts: [
            { text: "First part" },
            { text: "Second part" },
            { text: "Third part" },
          ],
        },
      },
    };
    expect(extractMessageText(body)).toBe("First part\nSecond part\nThird part");
  });

  it("ignores parts without text field", () => {
    const body = {
      params: {
        message: {
          parts: [{ text: "Hello" }, { other: "field" }, { text: "World" }],
        },
      },
    };
    expect(extractMessageText(body)).toBe("Hello\nWorld");
  });

  it("returns empty string when params.message is absent", () => {
    const body = { params: {} };
    expect(extractMessageText(body)).toBe("");
  });
});

describe("extractMessageText — response result format", () => {
  it("extracts text from result.parts[].text", () => {
    const body = {
      result: {
        parts: [{ text: "Agent response" }],
      },
    };
    expect(extractMessageText(body)).toBe("Agent response");
  });

  it("extracts text from result.parts[].root.text", () => {
    const body = {
      result: {
        parts: [{ root: { text: "Root response text" } }],
      },
    };
    expect(extractMessageText(body)).toBe("Root response text");
  });

  it("prefers parts[].text over parts[].root.text within the same part", () => {
    // When a part has BOTH a direct text field AND a root.text field,
    // direct text wins. Subsequent parts' root.text fields are ignored
    // when a direct text was found in an earlier part.
    const body = {
      result: {
        parts: [
          { text: "Direct text" },
          { root: { text: "Root text" } },
        ],
      },
    };
    expect(extractMessageText(body)).toBe("Direct text");
  });

  it("falls back to root.text when no direct text exists", () => {
    const body = {
      result: {
        parts: [{ root: { text: "Root only" } }],
      },
    };
    expect(extractMessageText(body)).toBe("Root only");
  });

  it("ignores subsequent parts root.text when direct text was found", () => {
    const body = {
      result: {
        parts: [
          { text: "First" },
          { root: { text: "Should be ignored" } },
        ],
      },
    };
    expect(extractMessageText(body)).toBe("First");
  });
});

describe("extractMessageText — plain string result", () => {
  it("returns body.result when it is a plain string", () => {
    const body = { result: "Simple string response" };
    expect(extractMessageText(body)).toBe("Simple string response");
  });
});

describe("extractMessageText — priority order", () => {
  it("prefers task format over params format", () => {
    const body = {
      task: "Task text",
      params: { message: { parts: [{ text: "Params text" }] } },
    };
    // Implementation: checks task first, returns if non-empty
    expect(extractMessageText(body)).toBe("Task text");
  });

  it("prefers params format over result format", () => {
    const body = {
      params: { message: { parts: [{ text: "Params text" }] } },
      result: { parts: [{ text: "Result text" }] },
    };
    // Implementation: checks params.message.parts first (after task)
    expect(extractMessageText(body)).toBe("Params text");
  });
});

describe("extractMessageText — error resilience", () => {
  it("returns empty string on malformed input", () => {
    expect(extractMessageText({})).toBe("");
    expect(extractMessageText({ params: null })).toBe("");
    expect(extractMessageText({ result: null })).toBe("");
  });

  it("returns empty string when all fields are absent", () => {
    expect(extractMessageText({ random: "field" })).toBe("");
  });

  it("handles missing parts array gracefully", () => {
    const body = { params: { message: {} } };
    expect(extractMessageText(body)).toBe("");
  });

  it("handles parts with undefined text gracefully", () => {
    const body = {
      result: {
        parts: [{ text: undefined }, { text: "valid" }],
      },
    };
    expect(extractMessageText(body)).toBe("valid");
  });
});
