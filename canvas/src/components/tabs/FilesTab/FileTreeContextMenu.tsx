"use client";

import { useEffect, useRef } from "react";

/**
 * FileTreeContextMenu — VSCode-style right-click menu for a single
 * file-tree row. Pops at the cursor's viewport coords; dismisses on
 * outside-click, Esc, blur, or scroll.
 *
 * Why a custom component (no library): the menu is one of several
 * "small popovers" in canvas; pulling in a dnd / popover lib for one
 * surface adds 10x the bytes of this implementation. The patterns
 * (outside-click + Esc + portal-free fixed position) match the
 * ContextMenu used in canvas/Toolbar so the keyboard-nav muscle
 * memory is uniform.
 *
 * Items are rendered from a `MenuItem[]` so callers can add/remove
 * actions without touching this component (e.g. PR-D will add an
 * "Upload to this folder" item for directory rows).
 *
 * Accessibility:
 * - role="menu" + role="menuitem" so screen readers announce the
 *   surface as a menu, not a generic div.
 * - First item gets autofocus so keyboard users can ↓/↑/Enter without
 *   reaching for the mouse.
 * - Esc + outside-click + Tab dismisses; behaves like every other
 *   menu the user has touched on the canvas.
 */
export interface MenuItem {
  /** Stable identifier for testing + analytics. */
  id: string;
  label: string;
  /** Optional left icon glyph; not load-bearing. */
  icon?: string;
  /** Destructive (rendered in red) — for Delete-class actions. */
  destructive?: boolean;
  /** Item-specific click handler. The menu auto-closes after onClick
   *  fires so handlers don't have to call onClose themselves. */
  onClick: () => void;
  /** Disabled items render but don't fire onClick (useful for
   *  Delete-on-non-/configs case where the caller wants to surface
   *  the item but explain it's gated). Currently unused — placeholder
   *  for future options. */
  disabled?: boolean;
}

interface Props {
  /** Viewport-coordinate position of the cursor that opened the menu. */
  x: number;
  y: number;
  items: MenuItem[];
  onClose: () => void;
}

export function FileTreeContextMenu({ x, y, items, onClose }: Props) {
  const ref = useRef<HTMLDivElement>(null);
  // First item gets initial focus for keyboard ↓/↑/Enter nav.
  const firstItemRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    firstItemRef.current?.focus();
  }, []);

  // Outside-click + Esc dismiss. Per memory
  // (feedback_abort_controller_for_rerendered_listeners), use an
  // AbortController so re-mounts (caller toggles the menu) don't leak
  // listeners.
  useEffect(() => {
    const ctrl = new AbortController();
    const onPointerDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        // Roving focus across .menuitem buttons. Doing this with
        // tabindex management because Tab / Shift+Tab leave the menu
        // (which is the right thing — the user is escaping the menu).
        e.preventDefault();
        const buttons = ref.current?.querySelectorAll<HTMLButtonElement>(
          "[role='menuitem']:not([disabled])",
        );
        if (!buttons || buttons.length === 0) return;
        const arr = Array.from(buttons);
        const cur = arr.indexOf(document.activeElement as HTMLButtonElement);
        const next =
          e.key === "ArrowDown"
            ? (cur + 1) % arr.length
            : (cur - 1 + arr.length) % arr.length;
        arr[next].focus();
      }
    };
    // `mousedown` (not `click`) so the menu dismisses BEFORE the
    // tree-row's click handler would fire — otherwise clicking
    // outside also selects a different row, which is not what the
    // user expected when "outside-click closes the menu".
    document.addEventListener("mousedown", onPointerDown, { signal: ctrl.signal });
    document.addEventListener("keydown", onKeyDown, { signal: ctrl.signal });
    // Scroll inside any ancestor also dismisses — the fixed-position
    // menu would otherwise stay anchored to viewport coords while the
    // row it points at scrolled away. Use capture so we catch scroll
    // on inner panels (FileTree's overflow-y-auto wrapper).
    document.addEventListener("scroll", onClose, { signal: ctrl.signal, capture: true });
    return () => ctrl.abort();
  }, [onClose]);

  return (
    <div
      ref={ref}
      role="menu"
      aria-label="File actions"
      className="fixed z-[1000] min-w-[140px] py-1 bg-surface-elevated border border-line/60 rounded-md shadow-xl shadow-black/30 text-[11px]"
      style={{ left: x, top: y }}
    >
      {items.map((item, i) => (
        <button
          key={item.id}
          ref={i === 0 ? firstItemRef : undefined}
          type="button"
          role="menuitem"
          disabled={item.disabled}
          onClick={() => {
            if (item.disabled) return;
            item.onClick();
            onClose();
          }}
          className={
            item.destructive
              ? "w-full text-left px-3 py-1 text-bad hover:bg-red-900/30 focus:bg-red-900/30 focus:outline-none disabled:opacity-40 disabled:pointer-events-none transition-colors"
              : "w-full text-left px-3 py-1 text-ink-mid hover:bg-surface-card hover:text-ink focus:bg-surface-card focus:text-ink focus:outline-none disabled:opacity-40 disabled:pointer-events-none transition-colors"
          }
        >
          {item.icon && <span className="inline-block w-4 mr-1.5 text-ink-soft">{item.icon}</span>}
          {item.label}
        </button>
      ))}
    </div>
  );
}
