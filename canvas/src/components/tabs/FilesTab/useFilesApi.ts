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
    async (fileList: FileList) => {
      let uploaded = 0;
      for (const file of Array.from(fileList)) {
        const path = file.webkitRelativePath || file.name;
        const parts = path.split("/");
        const relPath = parts.length > 1 ? parts.slice(1).join("/") : parts[0];
        if (file.size > 1_000_000) continue;
        try {
          const content = await file.text();
          await api.put(`/workspaces/${workspaceId}/files/${relPath}`, { content });
          uploaded++;
        } catch {
          /* skip binary */
        }
      }
      if (uploaded > 0) {
        useCanvasStore.getState().updateNodeData(workspaceId, { needsRestart: true });
        showToast(`Uploaded ${uploaded} files`, "success");
        loadFiles();
      }
      return uploaded;
    },
    [workspaceId, loadFiles]
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
    deleteAllFiles,
  };
}
