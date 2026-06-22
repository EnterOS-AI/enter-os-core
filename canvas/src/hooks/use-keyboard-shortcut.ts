import { useEffect } from 'react';

/**
 * Register a global keyboard shortcut.
 * Handles Cmd (Mac) / Ctrl (Win/Linux) modifier detection.
 */
export function useKeyboardShortcut(
  key: string,
  callback: () => void,
  opts: { meta?: boolean; ctrl?: boolean; enabled?: boolean } = {},
) {
  const { meta = false, ctrl = false, enabled = true } = opts;

  useEffect(() => {
    if (!enabled) return;

    function handler(e: KeyboardEvent) {
      const target = e.target as HTMLElement;
      if (target.closest?.('[data-display-stream="true"]')) return;
      if (e.key !== key) return;
      if (meta && !e.metaKey) return;
      if (ctrl && !e.ctrlKey) return;
      // Don't fire when typing in inputs (unless it's a shortcut combo)
      if (!meta && !ctrl) return;
      e.preventDefault();
      callback();
    }

    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [key, callback, meta, ctrl, enabled]);
}
