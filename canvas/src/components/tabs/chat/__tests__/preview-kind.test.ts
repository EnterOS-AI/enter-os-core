// preview-kind unit tests — exhaustive table of MIME / extension
// combinations. The kind helper is a pure function; this is the
// regression line for "what renders as what" across the entire chat
// surface.

import { describe, it, expect } from "vitest";
import { getAttachmentPreviewKind } from "../preview-kind";

describe("getAttachmentPreviewKind", () => {
  describe("strict MIME match", () => {
    const cases: Array<[string, ReturnType<typeof getAttachmentPreviewKind>]> = [
      // images
      ["image/png", "image"],
      ["image/jpeg", "image"],
      ["image/gif", "image"],
      ["image/webp", "image"],
      ["image/svg+xml", "image"],
      ["image/avif", "image"],
      ["IMAGE/PNG", "image"], // case-insensitive
      ["  image/png  ", "image"], // trim
      // video
      ["video/mp4", "video"],
      ["video/webm", "video"],
      ["video/quicktime", "video"],
      // audio
      ["audio/mpeg", "audio"],
      ["audio/wav", "audio"],
      ["audio/ogg", "audio"],
      // pdf
      ["application/pdf", "pdf"],
      // text family
      ["text/plain", "text"],
      ["text/markdown", "text"],
      ["text/html", "text"],
      ["text/css", "text"],
      ["text/javascript", "text"],
      ["text/csv", "text"],
      ["application/json", "text"],
      ["application/yaml", "text"],
      ["application/x-yaml", "text"],
      ["application/javascript", "text"],
      ["application/typescript", "text"],
      // unknown / non-renderable → file
      ["application/zip", "file"],
      ["application/octet-stream", "file"],
      ["application/x-tar", "file"],
      ["application/vnd.ms-excel", "file"],
      ["weird/unknown-thing", "file"],
    ];
    for (const [mime, expected] of cases) {
      it(`mimeType=${JSON.stringify(mime)} → ${expected}`, () => {
        expect(getAttachmentPreviewKind(mime)).toBe(expected);
      });
    }
  });

  describe("extension fallback when MIME is missing or generic", () => {
    const cases: Array<[string | undefined, string | undefined, string | undefined, ReturnType<typeof getAttachmentPreviewKind>]> = [
      // [mime, uri, name, expected]
      [undefined, "workspace:/tmp/screenshot.png", "screenshot.png", "image"],
      ["", "workspace:/tmp/photo.JPG", "photo.JPG", "image"],
      ["application/octet-stream", "workspace:/tmp/clip.mp4", "clip.mp4", "video"],
      [undefined, "workspace:/foo/song.mp3", "song.mp3", "audio"],
      [undefined, "workspace:/docs/report.pdf", "report.pdf", "pdf"],
      [undefined, "workspace:/code/main.py", "main.py", "text"],
      [undefined, "workspace:/data/notes.md", "notes.md", "text"],
      // No extension → file
      [undefined, "workspace:/tmp/Dockerfile", "Dockerfile", "file"],
      // Trailing dot → file
      [undefined, "workspace:/tmp/weird.", "weird.", "file"],
      // URL with query string + fragment → strip before parsing
      [undefined, "https://example.com/foo.png?download=1#anchor", "", "image"],
      // Unknown extension → file
      [undefined, "workspace:/tmp/something.xyz", "something.xyz", "file"],
      // Empty
      [undefined, "", "", "file"],
      [undefined, undefined, undefined, "file"],
    ];
    for (const [mime, uri, name, expected] of cases) {
      it(`mime=${mime ?? "<undef>"} uri=${uri} name=${name} → ${expected}`, () => {
        expect(getAttachmentPreviewKind(mime, uri, name)).toBe(expected);
      });
    }
  });

  describe("MIME wins over extension", () => {
    it("explicit mime=application/zip + extension=.png → file (don't render zip as image)", () => {
      // Critical safety: agent might attach a .png-named file that's
      // actually a zip. The strict-MIME branch wins and we render
      // the chip, not an <img> that 404s on broken bytes.
      expect(getAttachmentPreviewKind("application/zip", "x.png", "x.png")).toBe("file");
    });

    it("explicit mime=text/plain + extension=.png → text", () => {
      expect(getAttachmentPreviewKind("text/plain", "log.png", "log.png")).toBe("text");
    });
  });

  describe("regression: hostile-reviewer cases", () => {
    it("does NOT misclassify image/svg+xml as text (svg is image even though it has XML)", () => {
      expect(getAttachmentPreviewKind("image/svg+xml")).toBe("image");
    });

    it("application/octet-stream + extension=.docx → file (no renderer, don't try)", () => {
      expect(getAttachmentPreviewKind("application/octet-stream", "f.docx", "f.docx")).toBe("file");
    });

    it("non-canonical MIME application/json works", () => {
      expect(getAttachmentPreviewKind("application/json")).toBe("text");
    });
  });
});
