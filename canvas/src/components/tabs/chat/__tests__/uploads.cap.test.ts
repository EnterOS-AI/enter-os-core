import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  uploadChatFiles,
  FileTooLargeError,
  MAX_UPLOAD_BYTES,
  computeUploadTimeoutMs,
} from "../uploads";

// Tests for the 100 MB upload-cap raise + correct-reason error mapping
// (CTO 2026-05-19 directive on forensic a99ab0a1: "if its file size
// issue, should have error that instead saying timeout which is
// wrong"). Each case has its own specific reason; conflation is the
// bug this PR fixes.

// File constructor in node's vitest env supports size via array length.
// Allocate a typed-array of N bytes and wrap it — File reads .size off
// the underlying Blob. Allocating 101 MB once per test is fine (vitest
// maxWorkers=1, single test process).
function makeFile(name: string, size: number): File {
  const buf = new Uint8Array(size);
  return new File([buf], name);
}

const wsId = "00000000-0000-0000-0000-000000000001";

describe("uploadChatFiles — MAX_UPLOAD_BYTES + pre-flight gate", () => {
  it("MAX_UPLOAD_BYTES is exactly 100 MB (mirrors server constant)", () => {
    // Pinned so a regression that flipped the constant back to 50 MB
    // would fail loudly here — without this the canvas would
    // silently start rejecting files the server now accepts.
    expect(MAX_UPLOAD_BYTES).toBe(100 * 1024 * 1024);
  });

  it("throws FileTooLargeError for a 101 MB file BEFORE any network I/O", async () => {
    const oversize = makeFile("big.bin", 101 * 1024 * 1024);
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    try {
      await uploadChatFiles(wsId, [oversize]);
      throw new Error("expected uploadChatFiles to throw, but it resolved");
    } catch (e) {
      // The exact class name matters — useChatSend's mapUploadErrorToReason
      // routes off `instanceof FileTooLargeError`. A regression that
      // demoted to a plain Error would silently re-introduce the
      // wrong-reason conflation CTO flagged.
      expect(e).toBeInstanceOf(FileTooLargeError);
      const err = e as FileTooLargeError;
      // Message must contain the 100MB cap (so the user knows what the
      // limit is) and a number-with-MB form of the actual size.
      expect(err.message).toContain("100MB");
      // Some toFixed(1) renderings: 101.0MB. Loose match: contains "MB".
      expect(err.message).toMatch(/got\s+\d+(\.\d+)?MB/);
      expect(err.fileSize).toBe(101 * 1024 * 1024);
    }
    // CRITICAL: no fetch may have been initiated. Pre-flight is the
    // whole point — if a network round-trip happened we'd be back to
    // surfacing a downstream timeout / 413 instead of the actionable
    // file-size message.
    expect(fetchSpy).not.toHaveBeenCalled();
    fetchSpy.mockRestore();
  });

  it("accepts a file exactly at the cap (== MAX_UPLOAD_BYTES)", async () => {
    // Equality must NOT trip the gate — the cap is inclusive on the
    // server side and the canvas must match. Without this, an exact-
    // cap file would 503 client-side while the server accepts it.
    const exact = makeFile("max.bin", MAX_UPLOAD_BYTES);
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(
        new Response(JSON.stringify({ files: [] }), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
      );
    await expect(uploadChatFiles(wsId, [exact])).resolves.toBeDefined();
    expect(fetchSpy).toHaveBeenCalledOnce();
    fetchSpy.mockRestore();
  });
});

describe("computeUploadTimeoutMs — scaled timeout curve", () => {
  it("100 KB file → 60s floor (small-file ergonomics)", () => {
    // Below the floor, the small-file UX (typo'd hostname surfacing as
    // connect-error quickly) takes priority over the slow-uplink
    // assumption.
    expect(computeUploadTimeoutMs(100 * 1024)).toBe(60_000);
  });

  it("1 MB file → 60s floor", () => {
    expect(computeUploadTimeoutMs(1 * 1024 * 1024)).toBe(60_000);
  });

  it("100 MB file → ~1000s (matches the slow-uplink design budget)", () => {
    // Pin the upper-bound case the design targets: at 100 MB / 100 KB/s
    // a legitimate slow uplink completes in ~1000s, comfortably
    // before the platform's 1200s http.Client timeout. Without this
    // scaling, the previous fixed 60s deadline aborted Ryan's ~60 MB
    // upload in forensic a99ab0a1.
    const ms = computeUploadTimeoutMs(100 * 1024 * 1024);
    // 100*1024*1024 / 100 = 1048576 ms ≈ 1048.6s — pin to ±1ms.
    expect(ms).toBe(Math.floor((100 * 1024 * 1024) / 100));
    expect(ms).toBeGreaterThan(1_000_000);
    expect(ms).toBeLessThan(1_100_000);
  });

  it("strictly monotonic above the floor", () => {
    // A regression that capped or non-monotonised the curve would
    // silently re-introduce premature aborts for mid-size files.
    const a = computeUploadTimeoutMs(10 * 1024 * 1024);
    const b = computeUploadTimeoutMs(50 * 1024 * 1024);
    const c = computeUploadTimeoutMs(100 * 1024 * 1024);
    expect(b).toBeGreaterThan(a);
    expect(c).toBeGreaterThan(b);
  });
});

describe("uploadChatFiles — error path shapes (for downstream reason-mapping)", () => {
  let fetchSpy: ReturnType<typeof vi.spyOn> | null = null;

  beforeEach(() => {
    fetchSpy = vi.spyOn(globalThis, "fetch");
  });
  afterEach(() => {
    fetchSpy?.mockRestore();
    fetchSpy = null;
  });

  it("propagates the server's 413 reason verbatim (not as 'timeout')", async () => {
    // The error message text is what useChatSend surfaces via
    // `Upload failed: ${e.message}` — pin that the server's reason
    // is present, not swallowed.
    fetchSpy!.mockResolvedValue(
      new Response('{"error":"file exceeds per-file limit (100 MB)"}', {
        status: 413,
        headers: { "content-type": "application/json" },
      }),
    );
    const f = makeFile("small.bin", 1024);
    await expect(uploadChatFiles(wsId, [f])).rejects.toThrow(
      /upload failed:.*413.*per-file limit/i,
    );
  });

  it("propagates AbortSignal timeout as a DOMException with name=TimeoutError", async () => {
    // Reason-routing in useChatSend.mapUploadErrorToReason discriminates
    // by e.name === 'TimeoutError'. Pin the shape so a future browser /
    // polyfill change that renamed it would fail loudly here, NOT
    // silently fall through to the generic "Upload failed" path
    // (which is what made forensic a99ab0a1 hard to root-cause).
    const abortErr = new DOMException("signal timed out", "TimeoutError");
    fetchSpy!.mockRejectedValue(abortErr);
    const f = makeFile("small.bin", 1024);
    try {
      await uploadChatFiles(wsId, [f]);
      throw new Error("expected throw");
    } catch (e) {
      expect(e).toBeInstanceOf(DOMException);
      expect((e as DOMException).name).toBe("TimeoutError");
      // CRITICAL negative: the rejection must NOT be a
      // FileTooLargeError, because pre-flight already excluded that.
      expect(e).not.toBeInstanceOf(FileTooLargeError);
    }
  });

  it("a 50 MB file does NOT trip the pre-flight gate (sub-cap)", async () => {
    // The forensic case: Ryan's file was over the OLD 50MB cap but
    // under the NEW 100MB cap. Pin that the pre-flight does NOT
    // misfire on a sub-100MB file.
    fetchSpy!.mockResolvedValue(
      new Response('{"files":[]}', {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    const f = makeFile("ryan.bin", 50 * 1024 * 1024);
    await expect(uploadChatFiles(wsId, [f])).resolves.toBeDefined();
    expect(fetchSpy!).toHaveBeenCalledOnce();
  });
});
