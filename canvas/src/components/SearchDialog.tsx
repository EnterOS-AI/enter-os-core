"use client";

import { useState, useEffect, useRef, useCallback } from "react";
import { useCanvasStore } from "@/store/canvas";
import { statusDotClass } from "@/lib/design-tokens";

export function SearchDialog() {
  const open = useCanvasStore((s) => s.searchOpen);
  const setOpen = useCanvasStore((s) => s.setSearchOpen);
  const [query, setQuery] = useState("");
  const [focusedIndex, setFocusedIndex] = useState(-1);
  const inputRef = useRef<HTMLInputElement>(null);
  const nodes = useCanvasStore((s) => s.nodes);
  const selectNode = useCanvasStore((s) => s.selectNode);
  const setPanelTab = useCanvasStore((s) => s.setPanelTab);

  // Cmd+K to open
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setOpen(true);
        setQuery("");
      }
      if (e.key === "Escape" && open) {
        setOpen(false);
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open, setOpen]);

  useEffect(() => {
    if (open) {
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [open]);

  const filtered = nodes.filter((n) => {
    if (!query) return true;
    const q = query.toLowerCase();
    return (
      n.data.name.toLowerCase().includes(q) ||
      (n.data.role || "").toLowerCase().includes(q) ||
      n.data.status.toLowerCase().includes(q)
    );
  });

  // Auto-highlight the first match while the user is typing, so Enter
  // selects something instead of being a no-op. With an empty query we
  // keep -1 so opening the dialog (which shows ALL workspaces) doesn't
  // visually pin one row arbitrarily — only commit a highlight once the
  // user has narrowed the list.
  useEffect(() => {
    setFocusedIndex(query && filtered.length > 0 ? 0 : -1);
    // Re-running on filtered.length keeps the highlight pinned to the
    // first row while the result set shrinks/grows; the effect handler
    // above already short-circuits to -1 when results disappear.
  }, [query, filtered.length]);

  const handleSelect = useCallback(
    (nodeId: string) => {
      selectNode(nodeId);
      setPanelTab("details");
      setOpen(false);
    },
    [selectNode, setPanelTab, setOpen]
  );

  const handleInputKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLInputElement>) => {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setFocusedIndex((i) => Math.min(i + 1, filtered.length - 1));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setFocusedIndex((i) => Math.max(i - 1, 0));
      } else if (e.key === "Enter" && focusedIndex >= 0 && filtered[focusedIndex]) {
        e.preventDefault();
        handleSelect(filtered[focusedIndex].id);
      }
    },
    [filtered, focusedIndex, handleSelect]
  );

  const activeDescendant =
    focusedIndex >= 0 && filtered[focusedIndex]
      ? `search-result-${filtered[focusedIndex].id}`
      : undefined;

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-[70] flex items-start justify-center pt-[20vh] bg-black/50 backdrop-blur-sm"
      onClick={() => setOpen(false)}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Search workspaces"
        className="w-[420px] bg-surface/95 backdrop-blur-xl border border-line/60 rounded-2xl shadow-2xl shadow-black/50 overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Search input */}
        <div className="flex items-center gap-3 px-4 py-3 border-b border-line/40">
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none" className="shrink-0 text-ink-soft" aria-hidden="true">
            <circle cx="7" cy="7" r="5.5" stroke="currentColor" strokeWidth="1.5" />
            <path d="M11 11l3.5 3.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
          </svg>
          <input
            ref={inputRef}
            role="combobox"
            aria-label="Search workspaces"
            aria-expanded={filtered.length > 0}
            aria-autocomplete="list"
            aria-controls="search-results-list"
            aria-activedescendant={activeDescendant}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={handleInputKeyDown}
            placeholder="Search workspaces..."
            className="flex-1 bg-transparent text-sm text-ink placeholder-ink-soft focus:outline-none focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded"
          />
          <kbd className="text-[9px] text-ink-mid bg-surface-card/60 px-1.5 py-0.5 rounded border border-line/40">ESC</kbd>
        </div>

        {/* Results */}
        <div
          id="search-results-list"
          role="listbox"
          aria-label="Workspace results"
          className="max-h-[300px] overflow-y-auto py-1"
        >
          {filtered.length === 0 ? (
            <div role="status" aria-live="polite" className="px-4 py-6 text-center text-xs text-ink-mid">
              {query ? "No workspaces match" : "No workspaces yet"}
            </div>
          ) : (
            filtered.map((node, index) => (
              <button
                type="button"
                key={node.id}
                id={`search-result-${node.id}`}
                role="option"
                aria-selected={index === focusedIndex}
                onClick={() => handleSelect(node.id)}
                className={`w-full px-4 py-2.5 flex items-center gap-3 text-left transition-colors ${
                  index === focusedIndex ? "bg-surface-card/60" : "hover:bg-surface-card/40"
                }`}
              >
                <div
                  aria-hidden="true"
                  className={`w-2 h-2 rounded-full shrink-0 ${statusDotClass(node.data.status)}`}
                />
                <div className="min-w-0 flex-1">
                  <div className="text-sm text-ink truncate">{node.data.name}</div>
                  {node.data.role && (
                    <div className="text-[10px] text-ink-soft truncate">{node.data.role}</div>
                  )}
                </div>
                <span
                  className="text-[9px] font-mono text-ink-mid"
                  aria-label={`Tier ${node.data.tier}`}
                >
                  T{node.data.tier}
                </span>
              </button>
            ))
          )}
        </div>

        {/* Footer */}
        <div className="px-4 py-2 border-t border-line/40 flex items-center justify-between">
          <span className="text-[9px] text-ink-mid">{filtered.length} workspace{filtered.length !== 1 ? "s" : ""}</span>
          <div className="flex gap-2">
            <kbd className="text-[9px] text-ink-mid bg-surface-card/60 px-1.5 py-0.5 rounded border border-line/40">↑↓ navigate</kbd>
            <kbd className="text-[9px] text-ink-mid bg-surface-card/60 px-1.5 py-0.5 rounded border border-line/40">↵ select</kbd>
          </div>
        </div>
      </div>
    </div>
  );
}
