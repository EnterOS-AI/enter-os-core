'use client';
import { useState, useCallback, useEffect, useRef, useMemo } from 'react';
import { useSecretsStore } from '@/stores/secrets-store';
import { KeyValueField } from '@/components/ui/KeyValueField';
import { ValidationHint } from '@/components/ui/ValidationHint';
import { TestConnectionButton } from '@/components/ui/TestConnectionButton';
import {
  validateSecretValue,
  isValidKeyName,
  inferGroup,
} from '@/lib/validation/secret-formats';
import { SERVICES, KEY_NAME_SUGGESTIONS } from '@/lib/services';

const VALIDATION_DEBOUNCE_MS = 400;

interface AddKeyFormProps {
  workspaceId: string;
  existingNames: string[];
  onCancel: () => void;
}

/**
 * Inline-expanding form for adding a new API key.
 *
 * Design note (2026-04-22): the form used to open with a Service
 * dropdown (GitHub / Anthropic / OpenRouter / Other) gating what to
 * do next. That added friction — the storage layer only cares about
 * (key_name, value), and the provider can always be inferred from the
 * key name itself. We removed the dropdown and rely on:
 *
 *   - A datalist of common key-name suggestions so autocomplete
 *     replaces "pick a provider then the name auto-fills"
 *   - inferGroup(keyName) to classify the secret for validation +
 *     list-view grouping + test-connection routing, derived at render
 *     time from what the user actually typed
 *
 * Result: fewer fields, provider-agnostic by design, no UI code change
 * needed to onboard a new provider (MiniMax, DeepSeek, etc. just work
 * as soon as you type their canonical env var name).
 */
export function AddKeyForm({
  workspaceId,
  existingNames,
  onCancel,
}: AddKeyFormProps) {
  const createSecret = useSecretsStore((s) => s.createSecret);

  const [keyName, setKeyName] = useState('');
  const [value, setValue] = useState('');
  const [validationError, setValidationError] = useState<string | null>(null);
  const [keyNameError, setKeyNameError] = useState<string | null>(null);
  const [isSaving, setIsSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Group is derived, not selected. Falls back to 'custom' for any
  // key name that doesn't match a known provider pattern — validation
  // and test-connection still work, just without provider-specific
  // format hints.
  const inferredGroup = useMemo(() => inferGroup(keyName || ''), [keyName]);
  const service = SERVICES[inferredGroup];

  // Validate key name
  useEffect(() => {
    if (!keyName) {
      setKeyNameError(null);
      return;
    }
    if (!isValidKeyName(keyName)) {
      setKeyNameError('Key name must be UPPER_SNAKE_CASE');
      return;
    }
    if (existingNames.includes(keyName)) {
      setKeyNameError('A key named ' + keyName + ' already exists. Edit it instead.');
      return;
    }
    setKeyNameError(null);
  }, [keyName, existingNames]);

  // Debounced value validation against the inferred provider's format.
  useEffect(() => {
    if (!value) {
      setValidationError(null);
      return;
    }
    clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      setValidationError(validateSecretValue(value, inferredGroup));
    }, VALIDATION_DEBOUNCE_MS);
    return () => clearTimeout(debounceRef.current);
  }, [value, inferredGroup]);

  const handleSave = useCallback(async () => {
    if (!isValidKeyName(keyName)) {
      setKeyNameError('Key name must be UPPER_SNAKE_CASE');
      return;
    }
    const valErr = validateSecretValue(value, inferredGroup);
    if (valErr) {
      setValidationError(valErr);
      return;
    }

    setIsSaving(true);
    setSaveError(null);
    try {
      await createSecret(workspaceId, keyName, value);
      // Form auto-closes via store (isAddFormOpen set to false)
    } catch (e) {
      const message = e instanceof Error ? e.message : 'Failed to save. Check your connection and try again.';
      setSaveError(message);
    } finally {
      setIsSaving(false);
    }
  }, [keyName, value, inferredGroup, createSecret, workspaceId]);

  const canSave = keyName && value && !keyNameError && !validationError && !isSaving;

  // Show the provider-specific docs hint only when the key name
  // matches a known provider. For 'custom' (unknown key name) we stay
  // quiet — no false-structure prompt.
  const showProviderHint = inferredGroup !== 'custom' && service.docsUrl;

  return (
    <div className="add-key-form">
      <div className="add-key-form__header">Add New Key</div>

      {/* Key name — autocomplete replaces the old Service dropdown.
          inferGroup(keyName) derives classification at render time. */}
      <label className="add-key-form__label">
        Key name
        <input
          type="text"
          value={keyName}
          onChange={(e) => setKeyName(e.target.value.toUpperCase())}
          disabled={isSaving}
          placeholder="e.g. ANTHROPIC_API_KEY, MINIMAX_API_KEY, GITHUB_TOKEN"
          className="add-key-form__input"
          autoComplete="off"
          spellCheck={false}
          list="add-key-name-suggestions"
        />
      </label>
      <datalist id="add-key-name-suggestions">
        {KEY_NAME_SUGGESTIONS.map((name) => (
          <option key={name} value={name} />
        ))}
      </datalist>
      {keyNameError && <ValidationHint error={keyNameError} />}
      {showProviderHint && (
        <div className="add-key-form__hint" data-testid="provider-hint">
          <span className="add-key-form__hint-label">{service.label}</span>
          {' — '}
          <a
            href={service.docsUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="add-key-form__hint-link"
          >
            get a key
          </a>
        </div>
      )}

      {/* Key value */}
      <label className="add-key-form__label">
        Value
      </label>
      <KeyValueField
        value={value}
        onChange={setValue}
        disabled={isSaving}
        aria-label={`Value for ${keyName || 'new key'}`}
      />
      <ValidationHint
        error={validationError}
        showValid={!validationError && value.length > 0}
      />

      {/* Test connection (only when the inferred group supports it AND
          value looks format-valid). */}
      {service.testSupported && value && !validationError && (
        <TestConnectionButton
          provider={inferredGroup}
          secretValue={value}
        />
      )}

      {saveError && (
        <div className="add-key-form__error" role="alert">
          {saveError}
        </div>
      )}

      <div className="add-key-form__actions">
        <button
          type="button"
          onClick={onCancel}
          disabled={isSaving}
          className="add-key-form__cancel-btn"
        >
          Cancel
        </button>
        <button
          type="button"
          onClick={handleSave}
          disabled={!canSave}
          className="add-key-form__save-btn"
        >
          {isSaving ? 'Saving…' : 'Save key'}
        </button>
      </div>
    </div>
  );
}
