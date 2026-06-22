"use client";

interface RevealToggleProps {
  revealed: boolean;
  onToggle: () => void;
  label?: string;
}

/**
 * Eye / eye-off icon button for toggling secret visibility.
 * Uses semantic SVG icons — eye-open when hidden, eye-off when revealed.
 */
export function RevealToggle({
  revealed,
  onToggle,
  label = 'Toggle reveal secret',
}: RevealToggleProps) {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-label={label}
      className="reveal-toggle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1"
      title={revealed ? 'Hide value' : 'Show value'}
    >
      {revealed ? <EyeOffIcon /> : <EyeIcon />}
    </button>
  );
}

function EyeIcon() {
  return (
    <svg aria-hidden="true" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z" />
      <circle cx="12" cy="12" r="3" />
    </svg>
  );
}

function EyeOffIcon() {
  return (
    <svg aria-hidden="true" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94" />
      <path d="M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19" />
      <line x1="1" y1="1" x2="23" y2="23" />
    </svg>
  );
}
