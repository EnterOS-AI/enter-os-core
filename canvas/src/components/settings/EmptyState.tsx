'use client';

interface EmptyStateProps {
  onAddFirst: () => void;
}

/**
 * Shown when no secrets exist (replaces ServiceGroups).
 *
 * Per spec §3.2:
 *   🔑
 *   No API keys yet
 *   Add your API keys to let agents connect
 *   to GitHub, Anthropic, OpenRouter, and more.
 *   [+ Add your first API key]
 */
export function EmptyState({ onAddFirst }: EmptyStateProps) {
  return (
    <div className="empty-state">
      <div className="empty-state__icon" aria-hidden="true">
        🔑
      </div>
      <h3 className="empty-state__title">No API keys yet</h3>
      <p className="empty-state__body">
        Add your API keys to let agents connect to GitHub, Anthropic,
        OpenRouter, and more.
      </p>
      <button onClick={onAddFirst} className="empty-state__cta">
        + Add your first API key
      </button>
    </div>
  );
}
