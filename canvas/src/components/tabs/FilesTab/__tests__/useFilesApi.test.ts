// @vitest-environment jsdom
/**
 * useFilesApi.ts — walkEntry coverage only.
 *
 * The __testables import pulls in the full useFilesApi.ts module (355 lines,
 * imports react, @/lib/api, @/store/canvas). In the jsdom pool this can
 * OOM on complex mocks. Only the lightweight walkEntry file cases are
 * tested here.
 *
 * Covers:
 *   - walkEntry: file entry resolves with correct path and content
 *   - walkEntry: prefix handling
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { describe, expect, it } from "vitest";
import { __testables } from "../useFilesApi";

const { walkEntry } = __testables;

// ─── Helpers ─────────────────────────────────────────────────────────────────

interface CollectedEntry {
  file: File;
  relativePath: string;
}

function makeFile(name: string, content = "test content"): { entry: object; file: File } {
  const file = new File([content], name, { type: "text/plain" });
  const entry = {
    isFile: true,
    isDirectory: false,
    name,
    fullPath: "/" + name,
    file: (success: (f: File) => void) => success(file),
  };
  return { entry: entry as never, file };
}

// ─── walkEntry — file entries ─────────────────────────────────────────────────

describe("walkEntry — file entry", () => {
  it("resolves a file entry with its relative path", async () => {
    const { entry } = makeFile("notes.md", "hello world");
    const out: CollectedEntry[] = [];
    await walkEntry(entry as never, "", out);
    expect(out).toHaveLength(1);
    expect(out[0]!.relativePath).toBe("notes.md");
    expect(await out[0]!.file.text()).toBe("hello world");
  });

  it("uses the provided prefix in the relative path", async () => {
    const { entry } = makeFile("README.md");
    const out: CollectedEntry[] = [];
    await walkEntry(entry as never, "docs", out);
    expect(out[0]!.relativePath).toBe("docs/README.md");
  });

  it("preserves nested prefixes across calls", async () => {
    const { entry } = makeFile("index.ts");
    const out: CollectedEntry[] = [];
    await walkEntry(entry as never, "src/components", out);
    expect(out[0]!.relativePath).toBe("src/components/index.ts");
  });

  it("handles filenames with spaces", async () => {
    const { entry } = makeFile("my notes.txt", "content");
    const out: CollectedEntry[] = [];
    await walkEntry(entry as never, "", out);
    expect(out[0]!.relativePath).toBe("my notes.txt");
  });

  it("handles filenames with unicode", async () => {
    const { entry } = makeFile("日本語.txt", "data");
    const out: CollectedEntry[] = [];
    await walkEntry(entry as never, "", out);
    expect(out[0]!.relativePath).toBe("日本語.txt");
  });

  it("populates the File object with correct content", async () => {
    const { entry, file } = makeFile("config.yaml", "runtime: langgraph");
    const out: CollectedEntry[] = [];
    await walkEntry(entry as never, "", out);
    expect(out[0]!.file).toBe(file);
    expect(await out[0]!.file.text()).toBe("runtime: langgraph");
  });

  it("appends to existing entries array (non-destructive)", async () => {
    const { entry } = makeFile("extra.ts");
    const out: CollectedEntry[] = [{ file: new File(["preexisting"], "prev.ts"), relativePath: "prev.ts" }];
    await walkEntry(entry as never, "", out);
    expect(out).toHaveLength(2);
    expect(out[0]!.relativePath).toBe("prev.ts");
    expect(out[1]!.relativePath).toBe("extra.ts");
  });
});
