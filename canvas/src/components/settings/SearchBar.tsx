'use client';
import { useCallback, useRef, useEffect } from 'react';
import { useSecretsStore } from '@/stores/secrets-store';

/**
 * Client-side search/filter for secret key names.
 *
 * Per spec §9:
 * - Shown only when ≥4 secrets exist
 * - Filters KeyNameLabel text, case-insensitive, on every keystroke
 * - Escape clears search (does NOT close panel)
 * - Cmd+F / Ctrl+F focuses search when panel is open
 */
export function SearchBar() {
  const searchQuery = useSecretsStore((s) => s.searchQuery);
  const setSearchQuery = useSecretsStore((s) => s.setSearchQuery);
  const inputRef = useRef<HTMLInputElement>(null);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation(); // Don't close panel
        setSearchQuery('');
        inputRef.current?.blur();
      }
    },
    [setSearchQuery],
  );

  // Cmd+F / Ctrl+F focuses search field
  useEffect(() => {
    function handler(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === 'f') {
        e.preventDefault();
        inputRef.current?.focus();
      }
    }
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, []);

  return (
    <div className="search-bar">
      <span className="search-bar__icon" aria-hidden="true">🔍</span>
      <input
        ref={inputRef}
        type="text"
        value={searchQuery}
        onChange={(e) => setSearchQuery(e.target.value)}
        onKeyDown={handleKeyDown}
        placeholder="Search keys…"
        className="search-bar__input"
        aria-label="Search API keys"
      />
    </div>
  );
}
