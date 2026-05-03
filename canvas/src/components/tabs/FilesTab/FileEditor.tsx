"use client";

import { useRef } from "react";
import { getIcon } from "./tree";

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

  if (!selectedFile) {
    return (
      <div className="flex-1 flex items-center justify-center">
        <div className="text-center">
          <div className="text-2xl opacity-20 mb-2">📄</div>
          <p className="text-[10px] text-ink-soft">Select a file to edit</p>
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
            className="text-[10px] text-ink-soft hover:text-ink-mid"
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
        <div className="p-4 text-xs text-ink-soft">Loading...</div>
      ) : (
        <textarea
          ref={editorRef}
          value={editContent}
          readOnly={root !== "/configs"}
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
