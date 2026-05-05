'use client';

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { api } from "@/lib/api";
import type { MemoryEntry } from "@/components/MemoryInspectorPanel";

type Scope = "LOCAL" | "TEAM" | "GLOBAL";
const SCOPES: Scope[] = ["LOCAL", "TEAM", "GLOBAL"];

interface AddProps {
  open: boolean;
  mode: "add";
  workspaceId: string;
  defaultScope: Scope;
  defaultNamespace?: string;
  entry?: undefined;
  onClose: () => void;
  onSaved: () => void;
}

interface EditProps {
  open: boolean;
  mode: "edit";
  workspaceId: string;
  entry: MemoryEntry;
  defaultScope?: undefined;
  defaultNamespace?: undefined;
  onClose: () => void;
  onSaved: () => void;
}

type Props = AddProps | EditProps;

export function MemoryEditorDialog(props: Props) {
  const { open, mode, workspaceId, onClose, onSaved } = props;
  const dialogRef = useRef<HTMLDivElement>(null);
  const [mounted, setMounted] = useState(false);
  const [scope, setScope] = useState<Scope>("LOCAL");
  const [namespace, setNamespace] = useState("general");
  const [content, setContent] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setMounted(true);
  }, []);

  // Reset form whenever the dialog opens.
  useEffect(() => {
    if (!open) return;
    setError(null);
    setSaving(false);
    if (mode === "edit" && props.entry) {
      setScope(props.entry.scope);
      setNamespace(props.entry.namespace || "general");
      setContent(props.entry.content);
    } else if (mode === "add") {
      setScope(props.defaultScope);
      setNamespace(props.defaultNamespace || "general");
      setContent("");
    }
    // mode/props are stable per-open; intentional shallow deps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // Move focus into the dialog when it opens (WCAG SC 2.4.3).
  useEffect(() => {
    if (!open || !mounted) return;
    const raf = requestAnimationFrame(() => {
      dialogRef.current?.querySelector<HTMLElement>("textarea, input, select")?.focus();
    });
    return () => cancelAnimationFrame(raf);
  }, [open, mounted]);

  // Escape closes; Cmd/Ctrl-Enter saves.
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const handleSaveRef = useRef<() => void>(() => {});
  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onCloseRef.current();
      } else if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        handleSaveRef.current();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open]);

  const handleSave = async () => {
    if (saving) return;
    const trimmed = content.trim();
    if (!trimmed) {
      setError("Content cannot be empty");
      return;
    }
    setError(null);
    setSaving(true);
    try {
      if (mode === "add") {
        await api.post(`/workspaces/${workspaceId}/memories`, {
          content: trimmed,
          scope,
          namespace: namespace.trim() || "general",
        });
      } else {
        // PATCH only sends fields that changed. Content always changeable;
        // namespace only sent if it differs from the original (saves a
        // no-op write through redactSecrets + re-embed).
        const original = props.entry;
        const body: Record<string, string> = {};
        if (trimmed !== original.content) body.content = trimmed;
        const ns = namespace.trim() || "general";
        if (ns !== original.namespace) body.namespace = ns;
        if (Object.keys(body).length === 0) {
          // No-op edit — close without an HTTP round-trip.
          onSaved();
          onClose();
          return;
        }
        await api.patch(
          `/workspaces/${workspaceId}/memories/${encodeURIComponent(original.id)}`,
          body,
        );
      }
      onSaved();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed");
    } finally {
      setSaving(false);
    }
  };
  handleSaveRef.current = handleSave;

  if (!open || !mounted) return null;

  const titleId = "memory-editor-title";
  const isEdit = mode === "edit";

  return createPortal(
    <div className="fixed inset-0 z-[9999] flex items-center justify-center">
      <div className="absolute inset-0 bg-black/60 backdrop-blur-sm" onClick={onClose} />

      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="relative bg-surface-sunken border border-line rounded-xl shadow-2xl shadow-black/50 max-w-[480px] w-full mx-4 overflow-hidden"
      >
        <div className="px-5 py-4 space-y-3">
          <h3 id={titleId} className="text-sm font-semibold text-ink">
            {isEdit ? "Edit memory" : "Add memory"}
          </h3>

          {/* Scope */}
          <div className="space-y-1">
            <label className="text-[10px] text-ink-soft block" htmlFor="memory-editor-scope">
              Scope
            </label>
            {isEdit ? (
              <div
                id="memory-editor-scope"
                className="text-[12px] font-mono text-ink-mid bg-surface rounded px-2 py-1.5 border border-line/50"
                title="Scope is fixed on edit. To move a memory across scopes, delete and re-create it."
              >
                {scope}
              </div>
            ) : (
              <div className="flex items-center gap-1" id="memory-editor-scope" role="radiogroup" aria-label="Scope">
                {SCOPES.map((s) => (
                  <button
                    key={s}
                    type="button"
                    role="radio"
                    aria-checked={scope === s}
                    onClick={() => setScope(s)}
                    className={[
                      "px-3 py-1 text-[11px] rounded transition-colors",
                      scope === s
                        ? "bg-accent-strong text-white"
                        : "bg-surface-card text-ink-mid hover:text-ink",
                    ].join(" ")}
                  >
                    {s}
                  </button>
                ))}
              </div>
            )}
          </div>

          {/* Namespace */}
          <div className="space-y-1">
            <label htmlFor="memory-editor-namespace" className="text-[10px] text-ink-soft block">
              Namespace
            </label>
            <input
              id="memory-editor-namespace"
              type="text"
              value={namespace}
              onChange={(e) => setNamespace(e.target.value)}
              placeholder="general"
              className="w-full bg-surface border border-line/60 focus:border-accent/60 rounded px-2 py-1.5 text-[12px] text-ink placeholder-zinc-600 focus:outline-none transition-colors"
            />
          </div>

          {/* Content */}
          <div className="space-y-1">
            <label htmlFor="memory-editor-content" className="text-[10px] text-ink-soft block">
              Content
            </label>
            <textarea
              id="memory-editor-content"
              value={content}
              onChange={(e) => setContent(e.target.value)}
              rows={6}
              placeholder="What should the agent remember?"
              className="w-full bg-surface border border-line/60 focus:border-accent/60 rounded px-2 py-1.5 text-[12px] font-mono text-ink placeholder-zinc-600 focus:outline-none transition-colors resize-y min-h-[100px] max-h-[300px]"
            />
          </div>

          {error && (
            <div
              role="alert"
              aria-live="assertive"
              className="px-2 py-1.5 bg-red-950/30 border border-red-800/40 rounded text-[11px] text-bad"
            >
              {error}
            </div>
          )}
        </div>

        <div className="flex items-center justify-end gap-2 px-5 py-3 border-t border-line bg-surface/50">
          <button
            type="button"
            onClick={onClose}
            disabled={saving}
            className="px-3.5 py-1.5 text-[13px] text-ink-mid hover:text-ink bg-surface-card hover:bg-surface-elevated border border-line hover:border-line-soft rounded-lg transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={handleSave}
            disabled={saving}
            className="px-3.5 py-1.5 text-[13px] rounded-lg transition-colors bg-accent hover:bg-accent-strong text-white focus:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-sunken focus-visible:ring-accent/60 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {saving ? "Saving…" : isEdit ? "Save changes" : "Add memory"}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
