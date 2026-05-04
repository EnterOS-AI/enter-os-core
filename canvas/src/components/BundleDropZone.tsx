"use client";

import { useState, useCallback, useRef } from "react";
import { api } from "@/lib/api";

export function BundleDropZone() {
  const [isDragging, setIsDragging] = useState(false);
  const [importing, setImporting] = useState(false);
  const [result, setResult] = useState<{ status: string; name?: string } | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  /**
   * Core file processor — shared between drag-drop and keyboard file-picker
   * so both code paths have identical import behaviour (WCAG 2.1.1).
   */
  const processFile = useCallback(async (file: File) => {
    if (!file.name.endsWith(".bundle.json")) {
      setResult({ status: "error", name: "Only .bundle.json files are accepted" });
      setTimeout(() => setResult(null), 3000);
      return;
    }
    setImporting(true);
    try {
      const text = await file.text();
      const bundle = JSON.parse(text);
      const res = await api.post<{ workspace_id: string; name: string; status: string }>(
        "/bundles/import",
        bundle
      );
      setResult({ status: "success", name: res.name || bundle.name });
      setTimeout(() => setResult(null), 4000);
    } catch (e) {
      setResult({
        status: "error",
        name: e instanceof Error ? e.message : "Import failed",
      });
      setTimeout(() => setResult(null), 4000);
    } finally {
      setImporting(false);
    }
  }, []);

  const handleDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    if (e.dataTransfer.types.includes("Files")) {
      setIsDragging(true);
    }
  }, []);

  const handleDragLeave = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setIsDragging(false);
  }, []);

  const handleDrop = useCallback(async (e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setIsDragging(false);
    const file = Array.from(e.dataTransfer.files).find(
      (f) => f.name.endsWith(".bundle.json")
    );
    if (!file) {
      setResult({ status: "error", name: "Only .bundle.json files are accepted" });
      setTimeout(() => setResult(null), 3000);
      return;
    }
    await processFile(file);
  }, [processFile]);

  const handleFileInput = useCallback(async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    e.target.value = ""; // reset so the same file can be re-selected
    await processFile(file);
  }, [processFile]);

  return (
    <>
      {/* Hidden file input — keyboard / assistive-tech alternative to drag-drop (WCAG 2.1.1) */}
      <input
        ref={fileInputRef}
        id="bundle-file-input"
        type="file"
        accept=".bundle.json"
        className="sr-only"
        onChange={handleFileInput}
        aria-label="Import bundle file"
      />

      {/* Invisible drop zone covering the canvas */}
      <div
        className="fixed inset-0 z-10 pointer-events-none"
        style={{ pointerEvents: isDragging ? "auto" : "none" }}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
        onDrop={handleDrop}
      />

      {/* Global drag listener to detect file entering the window */}
      <div
        className="fixed inset-0 z-[5]"
        onDragOver={handleDragOver}
        style={{ pointerEvents: "none" }}
      />

      {/* Keyboard-accessible import button — visible on focus or hover so
           keyboard / AT users can trigger bundle import without drag-and-drop (WCAG 2.1.1) */}
      <button
        type="button"
        onClick={() => fileInputRef.current?.click()}
        aria-label="Import bundle file"
        aria-controls="bundle-file-input"
        className="sr-only focus:not-sr-only fixed bottom-20 right-4 z-30 px-3 py-1.5 bg-surface-sunken/90 border border-line/50 rounded-lg text-[10px] text-ink-mid hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent transition-colors"
      >
        📦 Import bundle
      </button>

      {/* Visual overlay when dragging — was hardcoded blue-950/blue-400
          which doesn't flip with theme. accent colors stay visually
          consistent with the rest of the canvas in both modes. */}
      {isDragging && (
        <div className="fixed inset-0 z-20 flex items-center justify-center bg-accent/15 backdrop-blur-sm border-2 border-dashed border-accent/40 pointer-events-none">
          <div className="bg-surface-sunken/95 border border-accent/50 rounded-2xl px-8 py-6 shadow-2xl text-center">
            <div className="text-3xl mb-2" aria-hidden="true">📦</div>
            <div className="text-sm font-semibold text-ink">Drop Bundle to Import</div>
            <div className="text-xs text-ink-soft mt-1">.bundle.json files only</div>
          </div>
        </div>
      )}

      {/* Importing indicator — role=status + aria-live so SR users hear
          "Importing bundle..." while the API call is in flight, not just
          the result toast that fires after. motion-safe:animate-spin
          respects prefers-reduced-motion (Tailwind's motion-safe variant
          gates animation on the user's OS setting). */}
      {importing && (
        <div
          role="status"
          aria-live="polite"
          className="fixed bottom-6 left-1/2 -translate-x-1/2 z-50 bg-surface-sunken/95 border border-line/60 rounded-xl px-5 py-3 shadow-2xl flex items-center gap-3"
        >
          <div
            aria-hidden="true"
            className="w-4 h-4 border-2 border-accent border-t-transparent rounded-full motion-safe:animate-spin"
          />
          <span className="text-sm text-ink">Importing bundle...</span>
        </div>
      )}

      {/* Result toast — role="status" announces import outcome to screen readers */}
      {result && (
        <div
          role="status"
          aria-live="polite"
          className={`fixed bottom-6 left-1/2 -translate-x-1/2 z-50 rounded-xl px-5 py-3 shadow-2xl text-sm ${
            result.status === "success"
              ? "bg-emerald-950/90 border border-emerald-700/50 text-emerald-200"
              : "bg-red-950/90 border border-red-700/50 text-red-200"
          }`}
        >
          {result.status === "success"
            ? `Imported "${result.name}" successfully`
            : result.name}
        </div>
      )}
    </>
  );
}
