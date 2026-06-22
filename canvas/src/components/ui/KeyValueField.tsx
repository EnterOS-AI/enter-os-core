'use client';

import { useState, useCallback, useRef, useEffect, type ChangeEvent } from 'react';
import { RevealToggle } from './RevealToggle';

const AUTO_HIDE_MS = 30_000;

interface KeyValueFieldProps {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  disabled?: boolean;
  /** ARIA label for the input. */
  'aria-label'?: string;
}

/**
 * Password-style input for secret values with reveal toggle.
 * Auto-trims whitespace on paste. Auto-hides revealed value after 30s.
 */
export function KeyValueField({
  value,
  onChange,
  placeholder = 'Enter secret value',
  disabled = false,
  'aria-label': ariaLabel = 'Secret value',
}: KeyValueFieldProps) {
  const [revealed, setRevealed] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Auto-hide after 30s of inactivity when revealed
  useEffect(() => {
    if (revealed) {
      timerRef.current = setTimeout(() => setRevealed(false), AUTO_HIDE_MS);
      return () => clearTimeout(timerRef.current);
    }
  }, [revealed]);

  const handleChange = useCallback(
    (e: ChangeEvent<HTMLInputElement>) => {
      // Auto-trim whitespace on paste
      onChange(e.target.value !== e.target.value.trim()
        ? e.target.value.trim()
        : e.target.value);
    },
    [onChange],
  );

  const handlePaste = useCallback(
    (e: React.ClipboardEvent<HTMLInputElement>) => {
      const pasted = e.clipboardData.getData('text');
      const trimmed = pasted.trim();
      if (trimmed !== pasted) {
        e.preventDefault();
        onChange(trimmed);
      }
    },
    [onChange],
  );

  return (
    <div className="key-value-field">
      <input
        type={revealed ? 'text' : 'password'}
        value={value}
        onChange={handleChange}
        onPaste={handlePaste}
        placeholder={placeholder}
        disabled={disabled}
        aria-label={ariaLabel}
        autoComplete="off"
        spellCheck={false}
      />
      <RevealToggle
        revealed={revealed}
        onToggle={() => setRevealed((r) => !r)}
        label={`Toggle reveal secret`}
      />
    </div>
  );
}
