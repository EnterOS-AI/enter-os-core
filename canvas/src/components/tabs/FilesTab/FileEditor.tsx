"use client";

import { useRef } from "react";
import { getIcon } from "./tree";

// secretShapeMarker is the canonical body the workspace-server Files
// API returns when a file's path OR content matched a credential
// regex (internal#425 RFC, Phase 2b — backed by
// workspace-server/internal/secrets.ScanBytes). The marker is a
// fixed prefix so the canvas can detect it without parsing JSON and
// without round-tripping the matched bytes through the editor (which
// would defeat the purpose — clipboard, browser history, log
// surfaces would all see them).
//
// Today (Phase 1 / before 2b ships) the backend returns 501 for the
// only root that uses this path, so the marker is dead code until
// 2b lands. Wiring it in now keeps the canvas + backend contracts
// aligned in one PR rather than a follow-up. The constant is
// importable so a future test can pin the exact string.
export const SECRET_SHAPE_DENIED_MARKER = "<denied: secret-shape>";

interface Props {
  selectedFile: string | null;
  fileContent: string;
  editContent: string;
  setEditContent: (v: string) => void;
  loadingFile: boolean;
  saving: boolean;
  success: string | null;
  root: string;
  onSave: () => void;
  onDownload: () => void;
}

export function FileEditor({
  selectedFile,
  fileContent,
  editContent,
  setEditContent,
  loadingFile,
  saving,
  success,
  root,
  onSave,
  onDownload,
}: Props) {
  const editorRef = useRef<HTMLTextAreaElement>(null);
  const isDirty = editContent !== fileContent;

  // internal#425 Phase 3: detect the secret-shape denial marker and
  // render a placeholder instead of the editor. The marker comes
  // from workspace-server Phase 2b (secrets.ScanBytes) which refuses
  // to surface the file's bytes. We deliberately don't expose
  // the matched pattern's Name here — the canvas just shows the
  // generic denial. The Files API log surface has the Pattern.Name
  // for operators who need to debug a false positive.
  const isSecretShapeDenied = fileContent === SECRET_SHAPE_DENIED_MARKER;

  // /agent-home is read-only from the canvas (Phase 2b ships read +
  // delete; Phase-2b-followup may add write). Edits to /configs are
  // unchanged. Until 2b ships, /agent-home returns 501 so this
  // read-only gate is also dead code, but wiring it in now keeps
  // the UI honest the moment 2b lands without a follow-up canvas PR.
  const isReadOnlyRoot = root !== "/configs";

  if (!selectedFile) {
    return (
      <div className="flex-1 flex items-center justify-center">
        <div className="text-center">
          <div className="text-2xl opacity-20 mb-2">📄</div>
          <p className="text-[10px] text-ink-mid">Select a file to edit</p>
        </div>
      </div>
    );
  }

  return (
    <>
      {/* File header */}
      <div className="flex items-center justify-between px-3 py-1.5 border-b border-line/40 bg-surface-sunken/20">
        <div className="flex items-center gap-1.5 min-w-0">
          <span className="text-[10px] opacity-50">{getIcon(selectedFile, false)}</span>
          <span className="text-[10px] font-mono text-ink-mid truncate">{selectedFile}</span>
          {isDirty && <span className="text-[9px] text-warm">modified</span>}
        </div>
        <div className="flex items-center gap-2">
          {success && <span className="text-[9px] text-good">{success}</span>}
          <button
            onClick={onDownload}
            aria-label="Download file"
            className="text-[10px] text-ink-mid hover:text-ink-mid"
          >
            ↓
          </button>
          {root === "/configs" && (
            <button
              onClick={onSave}
              disabled={!isDirty || saving}
              className="text-[10px] text-accent hover:text-accent disabled:opacity-30"
            >
              {saving ? "Saving..." : "Save"}
            </button>
          )}
        </div>
      </div>

      {/* Editor area */}
      {loadingFile ? (
        <div className="p-4 text-xs text-ink-mid">Loading...</div>
      ) : isSecretShapeDenied ? (
        // Files API refused to surface this file's bytes because its
        // path or content matched a credential regex
        // (workspace-server/internal/secrets, internal#425 Phase 2b).
        // We render a placeholder INSTEAD OF the textarea so the
        // matched bytes never enter the DOM. Clipboard / view-source
        // / element-inspector all see the placeholder, not the
        // credential.
        <div
          role="region"
          aria-label="File content denied"
          className="flex-1 flex items-center justify-center p-6 bg-surface"
        >
          <div className="max-w-md text-center space-y-2">
            <div className="text-2xl opacity-40">🛡️</div>
            <p className="text-[11px] font-mono text-warm">
              {SECRET_SHAPE_DENIED_MARKER}
            </p>
            <p className="text-[10px] text-ink-mid leading-relaxed">
              The platform refused to surface this file because its
              path or content matched a credential-shape pattern.
              The bytes never left the workspace container.
            </p>
            <p className="text-[10px] text-ink-mid leading-relaxed">
              If this is a false positive (test fixture, docs example,
              or content that happens to share a credential's shape),
              rename the file or adjust the content via the workspace
              terminal so the regex no longer matches, then refresh.
            </p>
          </div>
        </div>
      ) : (
        <textarea
          ref={editorRef}
          value={editContent}
          readOnly={isReadOnlyRoot}
          onChange={(e) => setEditContent(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "s") {
              e.preventDefault();
              onSave();
            }
            if (e.key === "Tab") {
              e.preventDefault();
              const el = editorRef.current;
              if (!el) return;
              const start = el.selectionStart;
              const end = el.selectionEnd;
              const val = editContent;
              const updated = val.substring(0, start) + "  " + val.substring(end);
              setEditContent(updated);
              requestAnimationFrame(() => {
                if (editorRef.current) {
                  editorRef.current.selectionStart = editorRef.current.selectionEnd = start + 2;
                }
              });
            }
          }}
          spellCheck={false}
          className="flex-1 w-full bg-surface p-3 text-[11px] font-mono text-ink leading-relaxed resize-none focus:outline-none"
          style={{ tabSize: 2 }}
        />
      )}
    </>
  );
}
