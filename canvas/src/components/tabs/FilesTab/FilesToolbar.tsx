"use client";

import { useRef } from "react";

interface Props {
  root: string;
  setRoot: (r: string) => void;
  fileCount: number;
  onNewFile: () => void;
  onUpload: (files: FileList) => void;
  onDownloadAll: () => void;
  onClearAll: () => void;
  onRefresh: () => void;
}

export function FilesToolbar({
  root,
  setRoot,
  fileCount,
  onNewFile,
  onUpload,
  onDownloadAll,
  onClearAll,
  onRefresh,
}: Props) {
  const uploadRef = useRef<HTMLInputElement>(null);

  return (
    <div className="flex items-center justify-between px-3 py-2 border-b border-line/40 bg-surface-sunken/30">
      <div className="flex items-center gap-2">
        <select
          value={root}
          onChange={(e) => setRoot(e.target.value)}
          aria-label="File root directory"
          className="text-[10px] bg-surface-card text-ink-mid border border-line rounded px-1.5 py-0.5 outline-none"
        >
          <option value="/configs">/configs</option>
          <option value="/home">/home</option>
          <option value="/workspace">/workspace</option>
          <option value="/plugins">/plugins</option>
        </select>
        <span className="text-[10px] text-ink-soft">{fileCount} files</span>
      </div>
      <div className="flex gap-1.5">
        {root === "/configs" && (
          <>
            <button type="button" onClick={onNewFile} aria-label="Create new file" className="text-[10px] text-accent hover:text-accent" title="Create new file">
              + New
            </button>
            <input
              ref={uploadRef}
              type="file"
              aria-label="Upload folder files"
              // @ts-expect-error webkitdirectory
              webkitdirectory=""
              multiple
              className="hidden"
              onChange={(e) => e.target.files && onUpload(e.target.files)}
            />
            <button type="button" onClick={() => uploadRef.current?.click()} aria-label="Upload folder" className="text-[10px] text-accent hover:text-accent" title="Upload folder">
              Upload
            </button>
          </>
        )}
        <button type="button" onClick={onDownloadAll} aria-label="Download all files" className="text-[10px] text-ink-soft hover:text-ink-mid" title="Download all files">
          Export
        </button>
        {root === "/configs" && (
          <button type="button" onClick={onClearAll} aria-label="Delete all files" className="text-[10px] text-bad/60 hover:text-bad" title="Delete all files">
            Clear
          </button>
        )}
        <button type="button" onClick={onRefresh} aria-label="Refresh file list" className="text-[10px] text-ink-soft hover:text-ink-mid" title="Refresh">
          ↻
        </button>
      </div>
    </div>
  );
}
