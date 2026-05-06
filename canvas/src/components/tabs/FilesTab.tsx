"use client";

import { useState, useEffect, useRef, useMemo } from "react";
import { showToast } from "../Toaster";
import type { WorkspaceNodeData } from "@/store/canvas";
import { FilesToolbar } from "./FilesTab/FilesToolbar";
import { FileTree } from "./FilesTab/FileTree";
import { FileEditor } from "./FilesTab/FileEditor";
import { NotAvailablePanel } from "./FilesTab/NotAvailablePanel";
import { useFilesApi } from "./FilesTab/useFilesApi";
import { buildTree } from "./FilesTab/tree";

// Re-exports preserved for external imports (e.g. tests importing from `../tabs/FilesTab`)
export { buildTree } from "./FilesTab/tree";
export type { TreeNode } from "./FilesTab/tree";

interface Props {
  workspaceId: string;
  /** Workspace metadata from the canvas store. Optional for back-compat
   *  with any caller that still mounts <FilesTab workspaceId=.../> without
   *  threading data through (legacy tests). When present, runtime gates
   *  the early-return below. Mirrors TerminalTab's prop shape (#2830). */
  data?: WorkspaceNodeData;
}

/** Runtimes whose filesystem the platform doesn't own. The canvas can't
 *  list/read/write files on these — the agent runs on the user's own
 *  hardware (mac laptop, mac mini, hermes-on-home-server) and reaches
 *  the platform via the heartbeat-based polling Phase 30 layer.
 *
 *  Keep narrow — only add a runtime here when its provisioner genuinely
 *  has no platform-owned filesystem. Otherwise the user loses access to
 *  a real surface (e.g. claude-code SaaS workspaces have files served
 *  by ListFiles via EIC; they belong on the rendering path, not here). */
const RUNTIMES_WITHOUT_FILES = new Set(["external"]);

export function FilesTab({ workspaceId, data }: Props) {
  // Early-return for runtimes whose filesystem is not platform-owned.
  // Skips the whole useFilesApi hook + tree render below — without this,
  // mounting the tab for an external workspace would issue a GET that
  // the platform can technically answer (it reads its own DB row, not
  // the user's machine), but every result row is fictional. Showing
  // "0 files / No config files yet" reads as a bug. The placeholder
  // makes the absence intentional and points the user at the right
  // surface (Chat).
  if (data && RUNTIMES_WITHOUT_FILES.has(data.runtime)) {
    return <NotAvailablePanel runtime={data.runtime} />;
  }
  return <PlatformOwnedFilesTab workspaceId={workspaceId} />;
}

function PlatformOwnedFilesTab({ workspaceId }: { workspaceId: string }) {
  const [root, setRoot] = useState("/configs");
  const [selectedFile, setSelectedFile] = useState<string | null>(null);
  const [fileContent, setFileContent] = useState("");
  const [editContent, setEditContent] = useState("");
  const [loadingFile, setLoadingFile] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [showNewFile, setShowNewFile] = useState(false);
  const [newFileName, setNewFileName] = useState("");
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [showDeleteAll, setShowDeleteAll] = useState(false);
  const successTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    return () => clearTimeout(successTimerRef.current);
  }, []);

  const {
    files,
    loading,
    loadFiles,
    expandedDirs,
    loadingDir,
    toggleDir,
    readFile,
    writeFile,
    deleteFile,
    downloadFileByPath,
    downloadAllFiles,
    uploadFiles,
    deleteAllFiles,
  } = useFilesApi(workspaceId, root);

  const tree = useMemo(() => buildTree(files), [files]);

  const openFile = async (path: string) => {
    setLoadingFile(true);
    setError(null);
    setSuccess(null);
    try {
      const res = await readFile(path);
      setSelectedFile(path);
      setFileContent(res.content);
      setEditContent(res.content);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to read file");
    } finally {
      setLoadingFile(false);
    }
  };

  const saveFile = async () => {
    if (!selectedFile) return;
    setSaving(true);
    setError(null);
    try {
      await writeFile(selectedFile, editContent);
      setFileContent(editContent);
      setSuccess("Saved");
      clearTimeout(successTimerRef.current);
      successTimerRef.current = setTimeout(() => setSuccess(null), 2000);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  };

  const confirmDeleteFile = async () => {
    if (!confirmDelete) return;
    setError(null);
    try {
      await deleteFile(confirmDelete);
      if (selectedFile === confirmDelete) {
        setSelectedFile(null);
        setFileContent("");
        setEditContent("");
      }
      loadFiles();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to delete");
    } finally {
      setConfirmDelete(null);
    }
  };

  const createFile = async () => {
    if (!newFileName.trim()) return;
    setError(null);
    try {
      await writeFile(newFileName.trim(), "");
      setShowNewFile(false);
      setNewFileName("");
      loadFiles();
      openFile(newFileName.trim());
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to create");
    }
  };

  const handleDownloadFile = () => {
    if (!selectedFile || !fileContent) return;
    const blob = new Blob([editContent], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = selectedFile.split("/").pop() || "file";
    a.click();
    URL.revokeObjectURL(url);
    showToast("Downloaded", "success");
  };

  const handleDeleteAll = async () => {
    setError(null);
    await deleteAllFiles();
    setSelectedFile(null);
    setFileContent("");
    setEditContent("");
  };

  const handleRootChange = (r: string) => {
    setRoot(r);
    setSelectedFile(null);
    setFileContent("");
    setEditContent("");
  };

  if (loading) {
    return <div className="p-4 text-xs text-ink-soft">Loading files...</div>;
  }

  return (
    <div className="flex flex-col h-full">
      <FilesToolbar
        root={root}
        setRoot={handleRootChange}
        fileCount={files.filter((f) => !f.dir).length}
        onNewFile={() => setShowNewFile(true)}
        onUpload={uploadFiles}
        onDownloadAll={downloadAllFiles}
        onClearAll={() => setShowDeleteAll(true)}
        onRefresh={() => loadFiles()}
      />

      {showDeleteAll && (
        // role=alertdialog so SR users hear this destructive prompt
        // immediately. Delete-All hovers DARKER (bg-red-700) — same AA
        // contrast trap that bit ConfirmDialog/ApprovalBanner. Cancel
        // lifts to surface-elevated instead of the prior no-op hover.
        <div role="alertdialog" aria-labelledby="files-delete-all-msg" className="mx-3 mt-2 px-3 py-2 bg-red-950/30 border border-red-800/40 rounded space-y-1.5">
          <p id="files-delete-all-msg" className="text-xs text-bad">Delete all {files.filter((f) => !f.dir).length} files? This cannot be undone.</p>
          <div className="flex gap-2">
            <button type="button" onClick={() => { handleDeleteAll(); setShowDeleteAll(false); }} className="px-2 py-0.5 bg-red-600 hover:bg-red-700 text-[10px] rounded text-white transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-red-500/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface">Delete All</button>
            <button type="button" onClick={() => setShowDeleteAll(false)} className="px-2 py-0.5 bg-surface-card hover:bg-surface-elevated hover:text-ink text-[10px] rounded text-ink-mid transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 focus-visible:ring-offset-1 focus-visible:ring-offset-surface">Cancel</button>
          </div>
        </div>
      )}

      {error && (
        <div role="alert" className="mx-3 mt-2 px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">{error}</div>
      )}

      {confirmDelete && (
        <div role="alertdialog" aria-labelledby="files-delete-one-msg" className="mx-3 mt-2 px-3 py-2 bg-amber-950/30 border border-amber-800/40 rounded space-y-1.5">
          <p id="files-delete-one-msg" className="text-xs text-warm">Delete <span className="font-mono">{confirmDelete}</span>{files.find((f) => f.path === confirmDelete && f.dir) ? " and all its contents" : ""}?</p>
          <div className="flex gap-2">
            <button type="button" onClick={confirmDeleteFile} className="px-2 py-0.5 bg-red-600 hover:bg-red-700 text-[10px] rounded text-white transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-red-500/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface">Delete</button>
            <button type="button" onClick={() => setConfirmDelete(null)} className="px-2 py-0.5 bg-surface-card hover:bg-surface-elevated hover:text-ink text-[10px] rounded text-ink-mid transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 focus-visible:ring-offset-1 focus-visible:ring-offset-surface">Cancel</button>
          </div>
        </div>
      )}

      <div className="flex flex-1 min-h-0">
        {/* File tree */}
        <div className="w-[180px] border-r border-line/40 overflow-y-auto shrink-0">
          {/* New file input */}
          {showNewFile && (
            <div className="px-2 py-1 border-b border-line/40">
              <input
                aria-label="New file path"
                value={newFileName}
                onChange={(e) => setNewFileName(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && createFile()}
                placeholder="path/file.md"
                autoFocus
                className="w-full bg-surface-card border border-line rounded px-1.5 py-0.5 text-[10px] text-ink font-mono focus:outline-none focus:border-accent"
              />
            </div>
          )}

          {files.length === 0 ? (
            <div className="px-3 py-4 text-[10px] text-ink-soft text-center">
              No config files yet
            </div>
          ) : (
            <FileTree
              nodes={tree}
              selectedPath={selectedFile}
              onSelect={openFile}
              // Delete is currently gated to /configs to match the
              // toolbar's New / Upload / Clear affordances. Context
              // menu and inline ✕ both honour the gate. PR-A made the
              // backend EIC delete work on all roots — keeping the
              // canvas gate conservative until we want to expose
              // /home /workspace deletion intentionally.
              onDelete={root === "/configs" ? setConfirmDelete : () => {}}
              onDownload={downloadFileByPath}
              canDelete={root === "/configs"}
              expandedDirs={expandedDirs}
              onToggleDir={toggleDir}
              loadingDir={loadingDir}
            />
          )}
        </div>

        {/* Editor */}
        <div className="flex-1 flex flex-col min-w-0">
          <FileEditor
            selectedFile={selectedFile}
            fileContent={fileContent}
            editContent={editContent}
            setEditContent={setEditContent}
            loadingFile={loadingFile}
            saving={saving}
            success={success}
            root={root}
            onSave={saveFile}
            onDownload={handleDownloadFile}
          />
        </div>
      </div>
    </div>
  );
}
