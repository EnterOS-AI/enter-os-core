// @vitest-environment jsdom
/**
 * Tests for extractReplyText — the A2A result-path text extractor used
 * in ChatTab.tsx.
 *
 * extractReplyText pulls the agent's text reply out of an A2A response.
 * Concatenates ALL text parts (joined with "\n") rather than returning
 * just the first. Claude Code and other runtimes commonly emit multi-
 * part text replies for long content (markdown tables, code blocks),
 * and the prior "first part wins" implementation silently truncated
 * the rest. Mirrors extractTextsFromParts in message-parser.ts.
 *
 * Note: extractReplyText is scoped to the result.parts + result.artifacts
 * path — unlike extractResponseText which also handles body.task / body.text /
 * body.response_preview. It is the correct extractor for live A2A
 * responses where the text lives on result.
 */
import { describe, expect, it } from "vitest";
import { extractReplyText } from "../ChatTab";

describe("extractReplyText — A2A result path", () => {
  it("returns empty string for undefined response", () => {
    expect(extractReplyText(undefined as never)).toBe("");
  });

  it("returns empty string for null result", () => {
    expect(extractReplyText({ result: null as never })).toBe("");
  });

  it("returns empty string when result has no parts or artifacts", () => {
    expect(extractReplyText({ result: {} })).toBe("");
  });

  it("returns empty string when parts array is empty", () => {
    expect(extractReplyText({ result: { parts: [] } })).toBe("");
  });

  it("extracts text from a single text part", () => {
    expect(
      extractReplyText({ result: { parts: [{ kind: "text", text: "Hello world" }] } })
    ).toBe("Hello world");
  });

  it("extracts text from v0.2 parts using type=text", () => {
    expect(
      extractReplyText({ result: { parts: [{ type: "text", text: "v0.2 reply" }] } })
    ).toBe("v0.2 reply");
  });

  it("prefers kind over type when both are present", () => {
    expect(
      extractReplyText({
        result: { parts: [{ kind: "text", type: "file", text: "kind wins" }] },
      })
    ).toBe("kind wins");
  });

  it("extracts text from result.status.message.parts (standard A2A Task shape)", () => {
    expect(
      extractReplyText({
        result: {
          status: {
            message: {
              parts: [{ kind: "text", text: "Agent final reply" }],
            },
          },
        },
      })
    ).toBe("Agent final reply");
  });

  it("combines result.parts, result.status.message.parts, and artifacts", () => {
    expect(
      extractReplyText({
        result: {
          parts: [{ kind: "text", text: "Top-level part" }],
          status: {
            message: {
              parts: [{ kind: "text", text: "Status message part" }],
            },
          },
          artifacts: [{ parts: [{ kind: "text", text: "Artifact part" }] }],
        },
      })
    ).toBe("Top-level part\nStatus message part\nArtifact part");
  });

  it("concatenates multiple text parts with newlines (no truncation)", () => {
    expect(
      extractReplyText({
        result: {
          parts: [
            { kind: "text", text: "# Header" },
            { kind: "text", text: "| Col |" },
            { kind: "text", text: "| --- |" },
            { kind: "text", text: "| Row |" },
          ],
        },
      })
    ).toBe("# Header\n| Col |\n| --- |\n| Row |");
  });

  it("skips non-text parts", () => {
    expect(
      extractReplyText({
        result: {
          parts: [
            { kind: "image", text: "should be ignored" },
            { kind: "text", text: "visible" },
            { kind: "file", text: "also ignored" },
          ],
        },
      })
    ).toBe("visible");
  });

  it("skips text parts with empty string", () => {
    expect(extractReplyText({ result: { parts: [{ kind: "text", text: "" }] } })).toBe("");
  });

  it("skips parts with missing text field", () => {
    expect(extractReplyText({ result: { parts: [{ kind: "text" }] } })).toBe("");
  });

  it("walks artifacts and collects their text parts", () => {
    expect(
      extractReplyText({
        result: {
          artifacts: [
            { parts: [{ kind: "text", text: "Artifact one" }] },
            { parts: [{ kind: "text", text: "Artifact two" }] },
          ],
        },
      })
    ).toBe("Artifact one\nArtifact two");
  });

  it("combines result.parts AND result.artifacts text (both sources)", () => {
    expect(
      extractReplyText({
        result: {
          parts: [{ kind: "text", text: "Summary" }],
          artifacts: [
            { parts: [{ kind: "text", text: "Detail block one" }] },
            { parts: [{ kind: "text", text: "Detail block two" }] },
          ],
        },
      })
    ).toBe("Summary\nDetail block one\nDetail block two");
  });

  it("artifacts are processed even when parts are empty", () => {
    expect(
      extractReplyText({
        result: {
          parts: [],
          artifacts: [{ parts: [{ kind: "text", text: "Only artifact" }] }],
        },
      })
    ).toBe("Only artifact");
  });

  it("artifacts with empty parts array contribute nothing", () => {
    expect(extractReplyText({ result: { artifacts: [{ parts: [] }] } })).toBe("");
  });

  it("multiple artifacts each contribute their text", () => {
    expect(
      extractReplyText({
        result: {
          artifacts: [
            { parts: [{ kind: "text", text: "A" }, { kind: "text", text: "B" }] },
            { parts: [{ kind: "text", text: "C" }] },
          ],
        },
      })
    ).toBe("A\nB\nC");
  });
});
