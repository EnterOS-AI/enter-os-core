import { describe, it, expect } from "vitest";
import { isPlatformAttachment, resolveAttachmentHref } from "../uploads";

describe("resolveAttachmentHref — URI scheme normalisation", () => {
  const wsId = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

  it("rewrites the canonical workspace:<path> scheme to /chat/download", () => {
    const url = resolveAttachmentHref(wsId, "workspace:/workspace/report.pdf");
    expect(url).toContain(`/workspaces/${wsId}/chat/download`);
    expect(url).toContain(encodeURIComponent("/workspace/report.pdf"));
  });

  it("accepts bare absolute container paths (some agents omit the scheme)", () => {
    const url = resolveAttachmentHref(wsId, "/workspace/report.pdf");
    expect(url).toContain(`/workspaces/${wsId}/chat/download`);
    expect(url).toContain(encodeURIComponent("/workspace/report.pdf"));
  });

  it("accepts file:/// URIs pointing into an allowed root", () => {
    const url = resolveAttachmentHref(wsId, "file:///workspace/report.pdf");
    expect(url).toContain(`/workspaces/${wsId}/chat/download`);
    expect(url).toContain(encodeURIComponent("/workspace/report.pdf"));
  });

  it("passes through HTTP(S) URIs unchanged so off-platform artefacts still render", () => {
    const external = "https://example.com/static/report.pdf";
    expect(resolveAttachmentHref(wsId, external)).toBe(external);
  });

  it("passes through container paths that are not under any allowed root", () => {
    // /etc/passwd looks like a path but isn't one of the allowed
    // roots — falling back to raw passthrough forces the caller into
    // the external-URL branch, which opens a new tab and lets the
    // browser refuse. Rewriting would 400 anyway server-side.
    expect(resolveAttachmentHref(wsId, "/etc/passwd")).toBe("/etc/passwd");
  });

  it("passes through unknown schemes unchanged", () => {
    expect(resolveAttachmentHref(wsId, "s3://bucket/key")).toBe("s3://bucket/key");
  });
});

// #2973 follow-up to #2968: cover the platform-pending: scheme branch
// (poll-mode chat uploads) + the isPlatformAttachment SSOT helper that
// the chip-download and markdown-link paths both consume.
//
// Pre-fix the platform-pending: URI fell through to the raw URI →
// browser saw an unhandled-protocol click → about:blank. The fix
// resolves it to the platform pending-uploads endpoint with auth
// headers attached.
describe("resolveAttachmentHref — platform-pending: scheme (poll-mode uploads)", () => {
  // Use a chat workspace ID that DIFFERS from the one in the URI, so
  // tests can verify which one the resolver uses. The forward-across-
  // workspace case is real production behavior — files dragged into one
  // workspace's chat can be referenced from another.
  const chatWs = "chat-ws-aaaaaaaa";
  const sourceWs = "source-ws-bbbbbbbb";

  it("resolves a well-formed platform-pending: URI to /pending-uploads/<file>/content", () => {
    const url = resolveAttachmentHref(
      chatWs,
      `platform-pending:${sourceWs}/file-12345`,
    );
    expect(url).toContain(`/workspaces/${sourceWs}/pending-uploads/file-12345/content`);
  });

  it("uses the URI's wsid, NOT the chat workspace_id (cross-workspace forwarding)", () => {
    // The two ids differ — this is the case PR #2968's commit
    // explicitly calls out. A regression that flipped this would
    // silently mis-route the download to the WRONG workspace's
    // pending-uploads store, returning 404 (or worse, leaking).
    const url = resolveAttachmentHref(
      chatWs,
      `platform-pending:${sourceWs}/file-xyz`,
    );
    expect(url).toContain(`/workspaces/${sourceWs}/`);
    expect(url).not.toContain(`/workspaces/${chatWs}/`);
  });

  it("falls back to raw URI when platform-pending: is missing the slash", () => {
    // Defensive: a URI that drifted from the expected wsid/fileid shape
    // returns raw rather than producing a broken /pending-uploads//
    // path. Pinned to detect a regression where a future "helpful"
    // change synthesizes empty wsid/fileID.
    expect(resolveAttachmentHref(chatWs, "platform-pending:no-slash")).toBe(
      "platform-pending:no-slash",
    );
  });

  it("falls back to raw URI when platform-pending: has empty fileID", () => {
    expect(resolveAttachmentHref(chatWs, "platform-pending:abc/")).toBe(
      "platform-pending:abc/",
    );
  });

  it("falls back to raw URI when platform-pending: has empty wsid", () => {
    expect(resolveAttachmentHref(chatWs, "platform-pending:/file-xyz")).toBe(
      "platform-pending:/file-xyz",
    );
  });

  it("regression: exact production repro from #2968 (reno-stars)", () => {
    // From the original PR #2968 body: the chat's markdown-link
    // override fell through on this exact shape and the browser
    // navigated to about:blank. Pin the post-fix output so a future
    // refactor can't reintroduce the original bug.
    const url = resolveAttachmentHref(
      "chat-ws",
      "platform-pending:d76977b1-uuid/bb0dcaf3-uuid",
    );
    expect(url).toContain("/workspaces/d76977b1-uuid/pending-uploads/bb0dcaf3-uuid/content");
    expect(url).not.toContain("chat-ws");
  });
});

describe("isPlatformAttachment", () => {
  it("returns true for platform-pending: URIs", () => {
    expect(isPlatformAttachment("platform-pending:abc/file")).toBe(true);
  });

  it("returns true even for malformed platform-pending: URIs", () => {
    // The helper is a SHAPE check — caller routes through
    // downloadChatFile and downloadChatFile handles the malformed case
    // downstream. Pinning so a future helper that "validates" the
    // wsid/fileID shape doesn't silently break the auth-attached
    // download flow for in-flight URIs.
    expect(isPlatformAttachment("platform-pending:no-slash")).toBe(true);
  });

  it("returns true for workspace:<allowed-root> URIs", () => {
    expect(isPlatformAttachment("workspace:/configs/foo")).toBe(true);
    expect(isPlatformAttachment("workspace:/workspace/x.pdf")).toBe(true);
  });

  it("returns true for file:///<allowed-root> URIs", () => {
    expect(isPlatformAttachment("file:///workspace/x")).toBe(true);
  });

  it("returns true for absolute paths under allowed roots", () => {
    expect(isPlatformAttachment("/home/user/x")).toBe(true);
    expect(isPlatformAttachment("/configs/y")).toBe(true);
  });

  it("returns FALSE for bare HTTPS URLs to other origins", () => {
    // Auth-leak class regression: a helper that always returned true
    // would attach workspace tokens to third-party requests. Pin
    // the negative case explicitly.
    expect(isPlatformAttachment("https://example.com/file")).toBe(false);
    expect(isPlatformAttachment("http://example.com/file")).toBe(false);
  });

  it("returns FALSE for non-allowlisted root paths", () => {
    expect(isPlatformAttachment("/etc/passwd")).toBe(false);
    expect(isPlatformAttachment("/var/log/x")).toBe(false);
    expect(isPlatformAttachment("/tmp/x")).toBe(false);
  });

  it("returns FALSE for empty string", () => {
    expect(isPlatformAttachment("")).toBe(false);
  });

  it("returns FALSE for unrecognised schemes", () => {
    expect(isPlatformAttachment("s3://bucket/key")).toBe(false);
    expect(isPlatformAttachment("ftp://server/file")).toBe(false);
  });
});
