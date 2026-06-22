// @vitest-environment jsdom
/**
 * Tests for the /agent-home root selector + per-runtime default-root
 * + secret-shape denial placeholder (internal#425 Phase 3).
 *
 * Separate file so the diff is reviewable as a unit and the existing
 * FilesToolbar / FileEditor / FilesTab tests don't have to grow
 * agent-home-specific cases. Once Phase 2b lands, the read-only +
 * 501-stub assertions here can be tightened (or moved into the main
 * test file as the agent-home root becomes a first-class affordance).
 */
import React from "react";
import { render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { FilesToolbar } from "../FilesToolbar";
import {
  FileEditor,
  SECRET_SHAPE_DENIED_MARKER,
} from "../FileEditor";

afterEach(cleanup);

describe("internal#425 Phase 3 — /agent-home root selector", () => {
  it("dropdown includes /agent-home as an option", () => {
    // Pins the affordance is in the DOM even pre-Phase-2b — the
    // canvas design freezes today, the backend lands the dispatch
    // later. Without this, a future refactor that drops the option
    // would silently regress the RFC's Phase 1 contract (canvas
    // visibility) without breaking any other test.
    render(
      <FilesToolbar
        root="/configs"
        setRoot={vi.fn()}
        fileCount={0}
        onNewFile={vi.fn()}
        onUpload={vi.fn()}
        onDownloadAll={vi.fn()}
        onClearAll={vi.fn()}
        onRefresh={vi.fn()}
      />,
    );
    const select = screen.getByRole("combobox", {
      name: /file root directory/i,
    }) as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toContain("/agent-home");
  });

  it("dropdown shows /agent-home as the SELECTED root when prop is /agent-home", () => {
    render(
      <FilesToolbar
        root="/agent-home"
        setRoot={vi.fn()}
        fileCount={0}
        onNewFile={vi.fn()}
        onUpload={vi.fn()}
        onDownloadAll={vi.fn()}
        onClearAll={vi.fn()}
        onRefresh={vi.fn()}
      />,
    );
    const select = screen.getByRole("combobox", {
      name: /file root directory/i,
    }) as HTMLSelectElement;
    expect(select.value).toBe("/agent-home");
  });
});

describe("internal#425 Phase 3 — secret-shape denial placeholder", () => {
  // Files API Phase 2b returns SECRET_SHAPE_DENIED_MARKER as the file
  // body when the file's path or content matched a credential regex.
  // The editor MUST render the marker as a placeholder, not pump it
  // through the textarea — that would put the marker (and any future
  // matched bytes if the backend contract changes) into the DOM
  // value, clipboard, and inspector.

  it("renders the denial placeholder INSTEAD of the textarea when fileContent is the marker", () => {
    render(
      <FileEditor
        selectedFile="agent/.openclaw/secrets.env"
        fileContent={SECRET_SHAPE_DENIED_MARKER}
        editContent={SECRET_SHAPE_DENIED_MARKER}
        setEditContent={vi.fn()}
        loadingFile={false}
        saving={false}
        success={null}
        root="/agent-home"
        onSave={vi.fn()}
        onDownload={vi.fn()}
      />,
    );
    // Placeholder region present
    expect(
      screen.getByRole("region", { name: /file content denied/i }),
    ).toBeTruthy();
    // Marker text visible (so a debugging operator sees the canonical
    // contract string without having to dig into the source).
    expect(screen.getByText(SECRET_SHAPE_DENIED_MARKER)).toBeTruthy();
    // Critically: NO textarea — the bytes never reach a controlled
    // input. A regression that re-introduces the textarea path would
    // make the matched marker (and any future content) selectable +
    // copyable.
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("renders the textarea normally when fileContent is regular content", () => {
    render(
      <FileEditor
        selectedFile="config.yaml"
        fileContent="name: openclaw\n"
        editContent="name: openclaw\n"
        setEditContent={vi.fn()}
        loadingFile={false}
        saving={false}
        success={null}
        root="/configs"
        onSave={vi.fn()}
        onDownload={vi.fn()}
      />,
    );
    expect(screen.getByRole("textbox")).toBeTruthy();
    expect(screen.queryByRole("region", { name: /file content denied/i }))
      .toBeNull();
  });

  it("/agent-home renders textarea READ-ONLY for non-denied content", () => {
    // Phase 2b ships read + delete on /agent-home; write semantics
    // are decided later. Until then, the canvas presents the editor
    // as read-only so a user can't type into a buffer that the
    // backend will refuse to PUT. Without this gate, the user would
    // edit, hit Save, get a 501, and lose their context for why.
    render(
      <FileEditor
        selectedFile=".openclaw/agent-card.json"
        fileContent='{"name":"openclaw"}'
        editContent='{"name":"openclaw"}'
        setEditContent={vi.fn()}
        loadingFile={false}
        saving={false}
        success={null}
        root="/agent-home"
        onSave={vi.fn()}
        onDownload={vi.fn()}
      />,
    );
    const textarea = screen.getByRole("textbox") as HTMLTextAreaElement;
    expect(textarea.readOnly).toBe(true);
  });

  it("/configs renders textarea WRITABLE (regression guard for the read-only gate)", () => {
    render(
      <FileEditor
        selectedFile="config.yaml"
        fileContent="name: x\n"
        editContent="name: x\n"
        setEditContent={vi.fn()}
        loadingFile={false}
        saving={false}
        success={null}
        root="/configs"
        onSave={vi.fn()}
        onDownload={vi.fn()}
      />,
    );
    const textarea = screen.getByRole("textbox") as HTMLTextAreaElement;
    expect(textarea.readOnly).toBe(false);
  });
});

describe("internal#425 Phase 3 — marker constant is the canonical string", () => {
  // The marker string is part of the canvas <-> workspace-server
  // contract. The workspace-server emits this exact body; the canvas
  // detects it by exact-equality. A typo on either side would
  // silently break detection — the canvas would render the literal
  // string in the textarea instead of the placeholder. Pin the
  // contract value here.
  it("matches the contract value '<denied: secret-shape>'", () => {
    expect(SECRET_SHAPE_DENIED_MARKER).toBe("<denied: secret-shape>");
  });
});
