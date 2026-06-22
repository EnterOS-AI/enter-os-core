"use client";

import { useCallback, useRef, useState, useEffect } from "react";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import { showToast } from "../../Toaster";
import type { FileEntry } from "./tree";

export function useFilesApi(workspaceId: string, root: string) {
  const [files, setFiles] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [expandedDirs, setExpandedDirs] = useState<Set<string>>(new Set());
  const [loadingDir, setLoadingDir] = useState<string | null>(null);
  const expandedDirsRef = useRef(expandedDirs);
  expandedDirsRef.current = expandedDirs;

  const loadFiles = useCallback(async (subPath = "", depth = 1) => {
    if (!subPath) setLoading(true);
    else setLoadingDir(subPath);
    try {
      const params = new URLSearchParams({ root, depth: String(depth) });
      if (subPath) params.set("path", subPath);
      const data = await api.get<FileEntry[]>(`/workspaces/${workspaceId}/files?${params}`);
      if (!subPath) {
        // Root load — replace all
        setFiles(data);
      } else {
        // Subfolder load — merge direct children only (preserve expanded grandchildren)
        setFiles((prev) => {
          const prefix = subPath + "/";
          // Remove only direct children of this subPath (not deeper descendants)
          const filtered = prev.filter((f) => {
            if (!f.path.startsWith(prefix)) return true;
            const remainder = f.path.slice(prefix.length);
            // Keep entries that are nested deeper (grandchildren of other expanded dirs)
            return remainder.includes("/");
          });
          const newFiles = data.map((f) => ({ ...f, path: subPath + "/" + f.path }));
          return [...filtered, ...newFiles];
        });
      }
    } catch {
      if (!subPath) setFiles([]);
    } finally {
      setLoading(false);
      setLoadingDir(null);
    }
  }, [workspaceId, root]);

  const toggleDir = useCallback((dirPath: string) => {
    const wasExpanded = expandedDirsRef.current.has(dirPath);
    setExpandedDirs((prev) => {
      const next = new Set(prev);
      if (next.has(dirPath)) {
        next.delete(dirPath);
      } else {
        next.add(dirPath);
      }
      return next;
    });
    if (!wasExpanded) {
      loadFiles(dirPath, 1);
    }
  }, [loadFiles]);

  useEffect(() => {
    setExpandedDirs(new Set());
    loadFiles();
  }, [loadFiles]);

  const readFile = useCallback(
    (path: string) =>
      api.get<{ content: string }>(`/workspaces/${workspaceId}/files/${path}?root=${encodeURIComponent(root)}`),
    [workspaceId, root]
  );

  const writeFile = useCallback(
    async (path: string, content: string) => {
      await api.put(`/workspaces/${workspaceId}/files/${path}`, { content });
      useCanvasStore.getState().updateNodeData(workspaceId, { needsRestart: true });
    },
    [workspaceId]
  );

  const deleteFile = useCallback(
    async (path: string) => {
      await api.del(`/workspaces/${workspaceId}/files/${path}`);
      useCanvasStore.getState().updateNodeData(workspaceId, { needsRestart: true });
    },
    [workspaceId]
  );

  /**
   * Fetch a file's content from the server and trigger a browser
   * download. Used by the right-click "Download" context-menu item
   * (PR-C of issue #2999) — distinct from `handleDownloadFile` in
   * FilesTab which downloads the CURRENTLY-OPEN-IN-EDITOR file from
   * the in-memory `editContent` buffer (so unsaved edits round-trip
   * to disk). This helper downloads the on-server content, suitable
   * for arbitrary tree rows the user hasn't opened.
   */
  const downloadFileByPath = useCallback(
    async (path: string) => {
      try {
        const res = await api.get<{ content: string }>(
          `/workspaces/${workspaceId}/files/${path}?root=${encodeURIComponent(root)}`,
        );
        // text/plain is correct for the canvas's text-only file
        // surface (config.yaml, prompts, skill markdown). Binary
        // files would need an Accept-arraybuffer path; the API
        // returns string today so this matches the wire shape.
        const blob = new Blob([res.content], { type: "text/plain" });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = path.split("/").pop() || "file";
        a.click();
        URL.revokeObjectURL(url);
        showToast(`Downloaded ${a.download}`, "success");
      } catch (e) {
        showToast(
          `Download failed: ${e instanceof Error ? e.message : "unknown error"}`,
          "error",
        );
      }
    },
    [workspaceId, root],
  );

  const downloadAllFiles = useCallback(async () => {
    const fileEntries = files.filter((f) => !f.dir);
    const results = await Promise.allSettled(
      fileEntries.map((f) =>
        api
          .get<{ content: string }>(`/workspaces/${workspaceId}/files/${f.path}`)
          .then((res) => ({ path: f.path, content: res.content }))
      )
    );
    const allFiles: Record<string, string> = {};
    for (const r of results) {
      if (r.status === "fulfilled") allFiles[r.value.path] = r.value.content;
    }
    const blob = new Blob([JSON.stringify(allFiles, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "workspace-files.json";
    a.click();
    URL.revokeObjectURL(url);
    showToast(`Downloaded ${Object.keys(allFiles).length} files`, "success");
  }, [files, workspaceId]);

  const uploadFiles = useCallback(
    async (fileList: FileList, targetDir = "") => {
      let uploaded = 0;
      for (const file of Array.from(fileList)) {
        const path = file.webkitRelativePath || file.name;
        const parts = path.split("/");
        // For folder picker: webkitRelativePath is "<picked-folder>/a/b.txt"
        // — strip the picked-folder prefix so files land flat under the
        // workspace's target dir, not under a redundant outer folder.
        const relPath = parts.length > 1 ? parts.slice(1).join("/") : parts[0];
        const finalPath = targetDir ? `${targetDir}/${relPath}` : relPath;
        if (file.size > 1_000_000) continue;
        try {
          const content = await file.text();
          await api.put(`/workspaces/${workspaceId}/files/${finalPath}`, { content });
          uploaded++;
        } catch {
          /* skip binary */
        }
      }
      if (uploaded > 0) {
        useCanvasStore.getState().updateNodeData(workspaceId, { needsRestart: true });
        showToast(`Uploaded ${uploaded} files${targetDir ? ` to ${targetDir}` : ""}`, "success");
        loadFiles();
      }
      return uploaded;
    },
    [workspaceId, loadFiles]
  );

  /**
   * Upload files dragged from the OS via the HTML5 DataTransferItemList
   * API. Unlike the folder-picker path (uploadFiles), this preserves
   * the dropped folder structure under `targetDir` — drag a "skills/"
   * folder onto the /configs/skills row and you get
   * /configs/skills/skills/* (the OUTER folder name is preserved
   * because the user explicitly chose to drop a NAMED folder, unlike
   * the folder-picker which always wraps the picked dir).
   *
   * Walks FileSystemDirectoryEntry recursively via webkitGetAsEntry.
   * VSCode/JupyterLab use the same primitive — there's no other
   * portable browser API for "drag a folder from OS". `webkit*`
   * naming is a Chromium relic; Firefox + Safari implement the same
   * surface.
   *
   * Returns the number of files uploaded so the caller can show a
   * tally / fail toast.
   */
  const uploadDataTransferItems = useCallback(
    async (items: DataTransferItemList, targetDir = "") => {
      const fileEntries = collectFileEntries(items);
      let uploaded = 0;
      for (const { file, relativePath } of await fileEntries) {
        if (file.size > 1_000_000) continue;
        const finalPath = targetDir
          ? `${targetDir}/${relativePath}`
          : relativePath;
        try {
          const content = await file.text();
          await api.put(`/workspaces/${workspaceId}/files/${finalPath}`, {
            content,
          });
          uploaded++;
        } catch {
          /* skip binary */
        }
      }
      if (uploaded > 0) {
        useCanvasStore
          .getState()
          .updateNodeData(workspaceId, { needsRestart: true });
        showToast(
          `Uploaded ${uploaded} file${uploaded === 1 ? "" : "s"}${targetDir ? ` to ${targetDir}` : ""}`,
          "success",
        );
        loadFiles();
      }
      return uploaded;
    },
    [workspaceId, loadFiles],
  );

  const deleteAllFiles = useCallback(async () => {
    let deleted = 0;
    for (const f of files) {
      if (f.dir) continue;
      try {
        await api.del(`/workspaces/${workspaceId}/files/${f.path}`);
        deleted++;
      } catch {
        /* skip */
      }
    }
    showToast(`Deleted ${deleted} files`, "info");
    loadFiles();
    return deleted;
  }, [files, workspaceId, loadFiles]);

  return {
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
    uploadDataTransferItems,
    deleteAllFiles,
  };
}

// ----- DataTransfer entry walker (PR-D) ---------------------------------

/**
 * Minimal subset of the FileSystem Entry API surface we use. The DOM
 * lib types this as FileSystemEntry / FileSystemFileEntry /
 * FileSystemDirectoryEntry but the relevant methods are callback-
 * based. Keep the shape narrow + explicit so the recursion below
 * type-checks without pulling in the full DOM lib types.
 */
interface FSEntry {
  isFile: boolean;
  isDirectory: boolean;
  name: string;
  fullPath: string;
  file?(success: (f: File) => void, fail?: (e: unknown) => void): void;
  createReader?(): { readEntries(success: (entries: FSEntry[]) => void): void };
}

interface CollectedEntry {
  file: File;
  /** Path relative to the dropped root (e.g. "skills/web-search/SKILL.md"
   *  for a dropped "skills/" folder containing web-search/SKILL.md). */
  relativePath: string;
}

/**
 * Walk a DataTransferItemList, returning every file entry as a flat
 * array keyed by the path relative to the originally-dropped item.
 * Folders dropped from the OS expand recursively; loose files
 * passthrough with name as the relative path.
 *
 * Skips items where webkitGetAsEntry() returns null — that's how
 * the browser signals a non-file payload (e.g. a dragged URL or
 * text snippet).
 */
async function collectFileEntries(
  items: DataTransferItemList,
): Promise<CollectedEntry[]> {
  const out: CollectedEntry[] = [];
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    if (item.kind !== "file") continue;
    // webkitGetAsEntry is the standardised name; older Firefox used
    // getAsEntry. Both Chromium + Firefox + Safari ship the webkit-
    // prefixed variant today. There's no non-prefixed alternative.
    const entry = (item as DataTransferItem & {
      webkitGetAsEntry?: () => FSEntry | null;
    }).webkitGetAsEntry?.();
    if (!entry) continue;
    await walkEntry(entry, "", out);
  }
  return out;
}

async function walkEntry(
  entry: FSEntry,
  prefix: string,
  out: CollectedEntry[],
): Promise<void> {
  const name = entry.name;
  const relPath = prefix ? `${prefix}/${name}` : name;
  if (entry.isFile && entry.file) {
    const file = await new Promise<File>((resolve, reject) => {
      entry.file!(resolve, reject);
    });
    out.push({ file, relativePath: relPath });
    return;
  }
  if (entry.isDirectory && entry.createReader) {
    const reader = entry.createReader();
    // readEntries returns up to ~100 at a time on Chromium; loop
    // until empty so large folders aren't truncated.
    let batch: FSEntry[] = [];
    do {
      batch = await new Promise<FSEntry[]>((resolve) =>
        reader.readEntries(resolve),
      );
      for (const child of batch) {
        await walkEntry(child, relPath, out);
      }
    } while (batch.length > 0);
  }
}

// Exported for direct testing — the recursion + readEntries batching
// is the part most likely to silently truncate a real folder upload.
export const __testables = { collectFileEntries, walkEntry };
