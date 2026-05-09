"use client";

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

interface ShortcutGroup {
  title: string;
  shortcuts: Array<{ keys: string[]; description: string }>;
}

const SHORTCUT_GROUPS: ShortcutGroup[] = [
  {
    title: "Canvas",
    shortcuts: [
      {
        keys: ["Esc"],
        description: "Close context menu, clear selection, or deselect",
      },
      {
        keys: ["Enter"],
        description: "Descend into selected node's first child",
      },
      {
        keys: ["Shift", "Enter"],
        description: "Ascend to selected node's parent",
      },
      {
        keys: ["Cmd", "]"],
        description: "Bring selected node forward in z-order",
      },
      {
        keys: ["Cmd", "["],
        description: "Send selected node backward in z-order",
      },
      {
        keys: ["Z"],
        description: "Zoom to fit the selected team and its sub-workspaces",
      },
    ],
  },
  {
    title: "Navigation",
    shortcuts: [
      {
        keys: ["⌘K"],
        description: "Open workspace search",
      },
      {
        keys: ["Palette"],
        description: "Open the template palette to deploy a new workspace",
      },
      {
        keys: ["Dbl-click"],
        description: "Zoom canvas to fit a team node and all its sub-workspaces",
      },
      {
        keys: ["Right-click"],
        description: "Open the workspace context menu",
      },
    ],
  },
  {
    title: "Agent",
    shortcuts: [
      {
        keys: ["Chat"],
        description: "Send a message or resume a running task",
      },
      {
        keys: ["Config"],
        description: "Edit skills, model, secrets, and runtime settings",
      },
      {
        keys: ["Audit"],
        description: "View the activity ledger for the selected workspace",
      },
    ],
  },
];

interface Props {
  open: boolean;
  onClose: () => void;
}

export function KeyboardShortcutsDialog({ open, onClose }: Props) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    setMounted(true);
  }, []);

  // Move focus into the dialog when it opens (WCAG 2.1 SC 2.4.3)
  useEffect(() => {
    if (!open || !mounted) return;
    const raf = requestAnimationFrame(() => {
      dialogRef.current?.querySelector<HTMLElement>("button")?.focus();
    });
    return () => cancelAnimationFrame(raf);
  }, [open, mounted]);

  // Keyboard: Escape closes, Tab is trapped
  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        onClose();
        return;
      }
      if (e.key === "Tab" && dialogRef.current) {
        const focusable = Array.from(
          dialogRef.current.querySelectorAll<HTMLElement>(
            'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
          )
        ).filter((el) => !el.hasAttribute("disabled"));
        if (focusable.length === 0) {
          e.preventDefault();
          return;
        }
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        if (e.shiftKey) {
          if (document.activeElement === first) {
            e.preventDefault();
            last.focus();
          }
        } else {
          if (document.activeElement === last) {
            e.preventDefault();
            first.focus();
          }
        }
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open, onClose]);

  if (!open || !mounted) return null;

  return createPortal(
    <div className="fixed inset-0 z-[9999] flex items-center justify-center">
      {/* Backdrop */}
      <div
        className="absolute inset-0 bg-black/60 backdrop-blur-sm"
        onClick={onClose}
      />

      {/* Dialog */}
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="keyboard-shortcuts-title"
        className="relative bg-surface border border-line rounded-xl shadow-2xl shadow-black/60 max-w-[480px] w-full mx-4 overflow-hidden max-h-[80vh] flex flex-col"
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-line shrink-0">
          <h2
            id="keyboard-shortcuts-title"
            className="text-sm font-semibold text-ink"
          >
            Keyboard Shortcuts
          </h2>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close keyboard shortcuts"
            className="w-7 h-7 flex items-center justify-center rounded-lg text-ink-mid hover:text-ink hover:bg-surface-sunken transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          >
            ×
          </button>
        </div>

        {/* Content */}
        <div className="overflow-y-auto p-5 space-y-5">
          {SHORTCUT_GROUPS.map((group) => (
            <div key={group.title}>
              <h3 className="text-[10px] font-semibold uppercase tracking-[0.2em] text-ink-soft mb-2.5">
                {group.title}
              </h3>
              <div className="space-y-2">
                {group.shortcuts.map((shortcut, i) => (
                  <div
                    key={i}
                    className="flex items-center justify-between gap-4"
                  >
                    <span className="text-[13px] text-ink-mid">
                      {shortcut.description}
                    </span>
                    <kbd className="flex items-center gap-0.5 shrink-0">
                      {shortcut.keys.map((k, j) => (
                        <span key={j} className="flex items-center gap-0.5">
                          {j > 0 && (
                            <span className="text-[9px] text-ink-soft mx-0.5">
                              +
                            </span>
                          )}
                          <span className="inline-flex items-center rounded-md border border-line/70 bg-surface-sunken/70 px-2 py-0.5 text-[11px] font-medium text-ink tabular-nums font-mono">
                            {k}
                          </span>
                        </span>
                      ))}
                    </kbd>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>

        {/* Footer */}
        <div className="px-5 py-3 border-t border-line bg-surface-sunken/30 shrink-0">
          <p className="text-[10px] text-ink-soft text-center">
            Press{" "}
            <kbd className="inline-flex items-center rounded border border-line/70 bg-surface-sunken/70 px-1.5 py-0.5 text-[10px] font-medium text-ink font-mono">
              Esc
            </kbd>{" "}
            to close
          </p>
        </div>
      </div>
    </div>,
    document.body
  );
}
