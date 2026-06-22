import { describe, it, expect } from "vitest";
import { mapUploadErrorToReason } from "../useChatSend";
import { FileTooLargeError } from "../../uploads";

// Pin the case-by-case error mapping (CTO 2026-05-19 directive on
// forensic a99ab0a1: each cause maps to ITS OWN message, no
// conflation). The four cases — FileTooLargeError, TimeoutError,
// other Error, non-Error — are the entire user-facing contract this
// PR ships; each gets a dedicated assertion so a regression that
// re-conflated them would surface here.

describe("mapUploadErrorToReason", () => {
  it("FileTooLargeError → surfaces the pre-flight message verbatim", () => {
    const err = new FileTooLargeError(
      101 * 1024 * 1024,
      "File too large (got 101.0MB) — limit is 100MB. Please use a smaller file.",
    );
    const out = mapUploadErrorToReason(err);
    // Verbatim, no "Upload failed:" prefix — the FileTooLargeError
    // message is already a complete, user-facing sentence.
    expect(out).toBe(err.message);
    expect(out).not.toMatch(/^Upload failed:/);
    // Must mention the cap so the user knows what to aim for.
    expect(out).toContain("100MB");
    // Must NOT mention timeout — wrong-reason conflation guard.
    expect(out.toLowerCase()).not.toContain("timeout");
    expect(out.toLowerCase()).not.toContain("connection");
  });

  it("TimeoutError → connection-too-slow message, NOT file-size", () => {
    const err = new DOMException("signal timed out", "TimeoutError");
    const out = mapUploadErrorToReason(err);
    // The user-facing reason matches the design contract: tells the
    // user the connection is slow, gives them the actionable retry
    // hint, and does NOT mention file-size (pre-flight already
    // excluded that — this is the case CTO flagged).
    expect(out).toContain("Upload timed out");
    expect(out).toContain("connection is too slow");
    // CRITICAL negatives — guard against the wrong-reason conflation.
    expect(out).not.toMatch(/100MB|file too large|File too large/);
  });

  it("plain Error from server (e.g. 413) → wraps with 'Upload failed:' + server reason", () => {
    // What uploadChatFiles throws when res.ok is false. The message
    // already encodes the status + body; the mapper just prefixes
    // "Upload failed:" so the chat error banner makes sense.
    const err = new Error("upload failed: 413 file exceeds per-file limit");
    const out = mapUploadErrorToReason(err);
    expect(out).toBe("Upload failed: upload failed: 413 file exceeds per-file limit");
    // Server's actual reason must survive — that's the whole
    // feedback_surface_actionable_failure_reason_to_user point.
    expect(out).toContain("413");
    expect(out).toContain("per-file limit");
  });

  it("non-Error throw → generic fallback", () => {
    // A string-throw (or a frozen object) is unusual but possible in
    // some catch paths. The fallback must NOT crash and must still
    // give the user a non-empty reason.
    expect(mapUploadErrorToReason("some random string")).toBe("Upload failed");
    expect(mapUploadErrorToReason(undefined)).toBe("Upload failed");
    expect(mapUploadErrorToReason(null)).toBe("Upload failed");
    expect(mapUploadErrorToReason(42)).toBe("Upload failed");
  });

  it("an AbortError that ISN'T a TimeoutError falls through to generic Error path", () => {
    // Belt-and-braces: a regression that loosened the name check to
    // ANY DOMException would silently rewrite legitimate AbortError
    // (user-initiated cancel) into "connection too slow". Pin the
    // narrow check.
    const err = new DOMException("user aborted", "AbortError");
    const out = mapUploadErrorToReason(err);
    // Falls through to non-Error branch (DOMException is not an Error
    // subclass in node's vitest environment); accept either generic
    // fallback or the Error-message form depending on the runtime.
    expect(out).not.toContain("connection is too slow");
    expect(out).not.toContain("File too large");
  });
});
