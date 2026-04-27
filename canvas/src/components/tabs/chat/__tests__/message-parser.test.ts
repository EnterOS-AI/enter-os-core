import { describe, it, expect } from "vitest";
import {
  extractRequestText,
  extractResponseText,
  extractAgentText,
  extractTextsFromParts,
  extractFilesFromTask,
} from "../message-parser";

describe("extractRequestText", () => {
  it("extracts text from standard A2A request_body", () => {
    const body = {
      params: {
        message: {
          role: "user",
          parts: [{ kind: "text", text: "Hello agent" }],
        },
      },
    };
    expect(extractRequestText(body)).toBe("Hello agent");
  });

  it("returns empty string for null body", () => {
    expect(extractRequestText(null)).toBe("");
  });

  it("returns empty string for empty object", () => {
    expect(extractRequestText({})).toBe("");
  });

  it("returns empty string when params missing", () => {
    expect(extractRequestText({ other: "data" })).toBe("");
  });

  it("returns empty string when message missing", () => {
    expect(extractRequestText({ params: {} })).toBe("");
  });

  it("returns empty string when parts empty", () => {
    expect(extractRequestText({ params: { message: { parts: [] } } })).toBe("");
  });

  it("extracts first part text only", () => {
    const body = {
      params: {
        message: {
          parts: [
            { kind: "text", text: "First" },
            { kind: "text", text: "Second" },
          ],
        },
      },
    };
    expect(extractRequestText(body)).toBe("First");
  });

  it("handles non-text parts gracefully", () => {
    const body = {
      params: {
        message: {
          parts: [{ kind: "image", data: "base64..." }],
        },
      },
    };
    expect(extractRequestText(body)).toBe("");
  });
});

describe("extractResponseText", () => {
  it("extracts from result string", () => {
    expect(extractResponseText({ result: "Hello!" })).toBe("Hello!");
  });

  it("extracts from result.parts[].text", () => {
    const body = {
      result: {
        parts: [{ kind: "text", text: "Response text" }],
      },
    };
    expect(extractResponseText(body)).toBe("Response text");
  });

  it("extracts from result.parts[].root.text", () => {
    const body = {
      result: {
        parts: [{ root: { text: "Root text" } }],
      },
    };
    expect(extractResponseText(body)).toBe("Root text");
  });

  it("extracts from task field", () => {
    expect(extractResponseText({ task: "Task text" })).toBe("Task text");
  });

  it("returns empty for empty object", () => {
    expect(extractResponseText({})).toBe("");
  });

  it("returns empty when result has no parts", () => {
    expect(extractResponseText({ result: { other: true } })).toBe("");
  });

  // Regression: Claude Code (and other long-reply runtimes) emits
  // multi-part text replies. The previous implementation returned
  // only the first part, silently truncating the rest. Observed
  // 2026-04-25 on a 15k-char Wave 1 brief that rendered as just the
  // markdown table header.
  it("joins all text parts when result.parts has multiple", () => {
    const body = {
      result: {
        parts: [
          { kind: "text", text: "# Header" },
          { kind: "text", text: "| Col |" },
          { kind: "text", text: "| --- |" },
          { kind: "text", text: "| Row |" },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("# Header\n| Col |\n| --- |\n| Row |");
  });

  it("joins all text parts across multiple artifacts", () => {
    const body = {
      result: {
        artifacts: [
          { parts: [{ kind: "text", text: "First artifact" }] },
          { parts: [{ kind: "text", text: "Second artifact" }] },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("First artifact\nSecond artifact");
  });

  it("joins all .root.text variants when present", () => {
    const body = {
      result: {
        parts: [
          { root: { text: "alpha" } },
          { root: { text: "beta" } },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("alpha\nbeta");
  });

  // Regression: when a response carries BOTH parts and artifacts
  // (Hermes tool-call replies do this — summary in parts, detail in
  // artifacts), the early-return-on-parts implementation silently
  // dropped the artifacts body. The collected-from-every-source
  // implementation must surface both.
  it("collects text from BOTH result.parts AND result.artifacts when both present", () => {
    const body = {
      result: {
        parts: [{ kind: "text", text: "Summary" }],
        artifacts: [
          { parts: [{ kind: "text", text: "Detail block one" }] },
          { parts: [{ kind: "text", text: "Detail block two" }] },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("Summary\nDetail block one\nDetail block two");
  });
});

describe("extractTextsFromParts", () => {
  it("extracts text parts with kind=text", () => {
    const parts = [
      { kind: "text", text: "Hello" },
      { kind: "text", text: "World" },
    ];
    expect(extractTextsFromParts(parts)).toBe("Hello\nWorld");
  });

  it("extracts text parts with type=text", () => {
    const parts = [{ type: "text", text: "Legacy format" }];
    expect(extractTextsFromParts(parts)).toBe("Legacy format");
  });

  it("returns null for non-array", () => {
    expect(extractTextsFromParts(null)).toBeNull();
    expect(extractTextsFromParts(undefined)).toBeNull();
    expect(extractTextsFromParts("string")).toBeNull();
  });

  it("returns null for empty array", () => {
    expect(extractTextsFromParts([])).toBeNull();
  });

  it("filters out non-text parts", () => {
    const parts = [
      { kind: "image", data: "..." },
      { kind: "text", text: "Only text" },
    ];
    expect(extractTextsFromParts(parts)).toBe("Only text");
  });
});

describe("extractFilesFromTask", () => {
  it("pulls A2A file parts out of a result", () => {
    const task = {
      parts: [
        { kind: "text", text: "here's the report" },
        {
          kind: "file",
          file: { name: "report.pdf", mimeType: "application/pdf", uri: "workspace:/reports/report.pdf", size: 4096 },
        },
      ],
    };
    const files = extractFilesFromTask(task);
    expect(files).toEqual([
      { name: "report.pdf", mimeType: "application/pdf", uri: "workspace:/reports/report.pdf", size: 4096 },
    ]);
  });

  it("recovers a filename from the URI when `name` is absent", () => {
    const task = {
      parts: [
        { kind: "file", file: { uri: "workspace:/workspace/out/graph.png" } },
      ],
    };
    const files = extractFilesFromTask(task);
    expect(files[0].name).toBe("graph.png");
  });

  it("skips file parts without a URI (inline bytes are not supported yet)", () => {
    const task = {
      parts: [
        { kind: "file", file: { name: "inline.bin", bytes: "AAA=" } },
      ],
    };
    expect(extractFilesFromTask(task)).toEqual([]);
  });

  it("walks artifacts[] so file parts nested inside artifact envelopes are found", () => {
    const task = {
      artifacts: [
        {
          parts: [
            { kind: "file", file: { name: "trace.log", uri: "workspace:/logs/trace.log" } },
          ],
        },
      ],
    };
    const files = extractFilesFromTask(task);
    expect(files[0]).toMatchObject({ name: "trace.log", uri: "workspace:/logs/trace.log" });
  });

  it("returns [] on malformed input rather than throwing", () => {
    expect(extractFilesFromTask({})).toEqual([]);
    expect(extractFilesFromTask({ parts: "not-an-array" } as unknown as Record<string, unknown>)).toEqual([]);
  });

  it("walks result.message.parts — the non-task reply shape some A2A servers use", () => {
    const task = {
      message: {
        parts: [
          { kind: "file", file: { name: "out.txt", uri: "workspace:/workspace/out.txt" } },
        ],
      },
    };
    const files = extractFilesFromTask(task);
    expect(files[0]).toMatchObject({ name: "out.txt", uri: "workspace:/workspace/out.txt" });
  });

  it("hydrates a notify-with-attachments response_body — both text caption AND file chips", () => {
    // Pins the exact wire shape the platform's Notify handler persists
    // when send_message_to_user passes attachments (activity.go writes
    // response_body = {"result": <message>, "parts": [{kind:"file",...}]}).
    // The chat history loader runs both extractors over this object on
    // reload — without this contract holding, refreshing the page after
    // an agent attached a file would lose either the caption or the chips.
    const responseBody = {
      result: "Done — see attached.",
      parts: [
        {
          kind: "file",
          file: {
            name: "build-output.zip",
            mimeType: "application/zip",
            uri: "workspace:/tmp/build-output.zip",
            size: 12345,
          },
        },
      ],
    };
    expect(extractResponseText(responseBody)).toBe("Done — see attached.");
    expect(extractFilesFromTask(responseBody)).toEqual([
      {
        name: "build-output.zip",
        mimeType: "application/zip",
        uri: "workspace:/tmp/build-output.zip",
        size: 12345,
      },
    ]);
  });
});
