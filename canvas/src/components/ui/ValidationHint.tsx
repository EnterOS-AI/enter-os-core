interface ValidationHintProps {
  /** null = valid / not yet validated. string = error message. */
  error: string | null;
  /** Shown when value is valid and non-empty. */
  showValid?: boolean;
}

/**
 * Inline validation feedback below a form field.
 * - Error: red text + ⚠ icon
 * - Valid: green text + ✓
 * - Neutral (null, not yet typed): hidden
 *
 * Per spec: 12px caption text, placed below the value field.
 */
export function ValidationHint({ error, showValid = false }: ValidationHintProps) {
  if (error) {
    return (
      <p className="validation-hint validation-hint--error" role="alert">
        <span aria-hidden="true">⚠</span> {error}
      </p>
    );
  }

  if (showValid) {
    return (
      <p className="validation-hint validation-hint--valid">
        <span aria-hidden="true">✓</span> Valid format
      </p>
    );
  }

  return null;
}
