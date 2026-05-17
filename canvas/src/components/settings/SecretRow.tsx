'use client';
import { useState, useCallback, useRef, useEffect } from 'react';
import type { Secret, SecretGroup } from '@/types/secrets';
import { useSecretsStore } from '@/stores/secrets-store';
import { StatusBadge } from '@/components/ui/StatusBadge';
import { KeyValueField } from '@/components/ui/KeyValueField';
import { ValidationHint } from '@/components/ui/ValidationHint';
import { TestConnectionButton } from '@/components/ui/TestConnectionButton';
import { validateSecretValue } from '@/lib/validation/secret-formats';
import { SERVICES } from '@/lib/services';

const VALIDATION_DEBOUNCE_MS = 400;

// Secret values are write-only from the browser: the server List endpoint
// "Never exposes values", there is no per-secret decrypt route, and the
// only decrypted path (GET /secrets/values) is bulk + token-gated for
// remote agents. The old eye/RevealToggle was a dead affordance — it
// flipped its own icon but could never reveal anything, which read as
// "this doesn't work" (esp. once clicked → eye-with-slash). We show an
// honest static indicator instead; rotation is via Edit.
const WRITE_ONLY_TITLE =
  'Value is write-only and cannot be revealed — use Edit to replace/rotate it';

interface SecretRowProps {
  secret: Secret;
  workspaceId: string;
}

/**
 * Single secret display row with masked value, status, and inline edit form.
 *
 * Display mode: key name | masked value | [reveal] [status] [copy] [edit] [delete]
 * Edit mode: row expands to show value input + validation + test + save/cancel
 */
export function SecretRow({ secret, workspaceId }: SecretRowProps) {
  const editingKey = useSecretsStore((s) => s.editingKey);
  const setEditingKey = useSecretsStore((s) => s.setEditingKey);
  const updateSecret = useSecretsStore((s) => s.updateSecret);
  const setSecretStatus = useSecretsStore((s) => s.setSecretStatus);

  const isEditing = editingKey === secret.name;
  const [editValue, setEditValue] = useState('');
  const [validationError, setValidationError] = useState<string | null>(null);
  const [isSaving, setIsSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  const editBtnRef = useRef<HTMLButtonElement>(null);

  // Debounced validation
  useEffect(() => {
    if (!isEditing || !editValue) {
      setValidationError(null);
      return;
    }
    debounceRef.current = setTimeout(() => {
      setValidationError(validateSecretValue(editValue, secret.group));
    }, VALIDATION_DEBOUNCE_MS);
    return () => clearTimeout(debounceRef.current);
  }, [editValue, isEditing, secret.group]);

  const handleEdit = useCallback(() => {
    setEditValue('');
    setSaveError(null);
    setValidationError(null);
    setEditingKey(secret.name);
  }, [setEditingKey, secret.name]);

  const handleCancel = useCallback(() => {
    setEditingKey(null);
    setEditValue('');
    editBtnRef.current?.focus();
  }, [setEditingKey]);

  const handleSave = useCallback(async () => {
    // Validate on submit
    const err = validateSecretValue(editValue, secret.group);
    if (err) {
      setValidationError(err);
      return;
    }
    setIsSaving(true);
    setSaveError(null);
    try {
      await updateSecret(workspaceId, secret.name, editValue);
      // Status resets to unverified after update (per spec §4.3)
      setSecretStatus(secret.name, 'unverified');
      editBtnRef.current?.focus();
    } catch (e) {
      setSaveError(
        e instanceof Error ? e.message : 'Failed to save. Try again.',
      );
    } finally {
      setIsSaving(false);
    }
  }, [editValue, secret.group, secret.name, updateSecret, workspaceId, setSecretStatus]);

  const handleCopy = useCallback(async () => {
    // Per spec: copy sends full value server-side when masked.
    // For now, copy the masked value (real implementation would
    // fetch plaintext from a dedicated endpoint).
    await navigator.clipboard.writeText(secret.masked_value);
  }, [secret.masked_value]);

  const handleDelete = useCallback(() => {
    // Trigger delete flow — this is handled by parent via DeleteConfirmDialog
    useSecretsStore.getState().setEditingKey(null);
    // Emit custom event for DeleteConfirmDialog to pick up
    window.dispatchEvent(
      new CustomEvent('secret:delete-request', { detail: secret.name }),
    );
  }, [secret.name]);

  const service = SERVICES[secret.group];

  return (
    <div
      className={`secret-row ${isEditing ? 'secret-row--editing' : ''}`}
      role="row"
      aria-label={`${secret.name} — ${service.label} — ${secret.status}`}
    >
      {/* Display mode */}
      <div className="secret-row__display">
        <span className="secret-row__name">{secret.name}</span>
        <span className="secret-row__value">
          {secret.masked_value}
        </span>
        <div className="secret-row__actions">
          <span
            data-testid="write-only-indicator"
            className="secret-row__write-only"
            role="img"
            aria-label={`${secret.name} value is write-only and cannot be revealed; use Edit to replace it`}
            title={WRITE_ONLY_TITLE}
          >
            🔒
          </span>
          <StatusBadge status={secret.status} />
          <button
            type="button"
            onClick={handleCopy}
            aria-label={`Copy ${secret.name} to clipboard`}
            className="secret-row__action-btn"
            title="Copy"
          >
            ⎘
          </button>
          <button
            type="button"
            ref={editBtnRef}
            onClick={handleEdit}
            aria-label={`Edit ${secret.name}`}
            className="secret-row__action-btn"
            title="Edit"
          >
            ✏
          </button>
          <button
            type="button"
            onClick={handleDelete}
            aria-label={`Delete ${secret.name}`}
            className="secret-row__action-btn secret-row__action-btn--delete"
            title="Delete"
          >
            🗑
          </button>
        </div>
      </div>

      {/* Edit mode — inline expand */}
      {isEditing && (
        <div className="secret-row__edit-form">
          <p className="secret-row__edit-hint">
            Enter new value to replace — current value not shown for security
          </p>
          <KeyValueField
            value={editValue}
            onChange={setEditValue}
            disabled={isSaving}
            aria-label={`New value for ${secret.name}`}
          />
          <ValidationHint
            error={validationError}
            showValid={!validationError && editValue.length > 0}
          />
          {service.testSupported && editValue && !validationError && (
            <TestConnectionButton
              provider={secret.group}
              secretValue={editValue}
              onResult={(valid) =>
                setSecretStatus(secret.name, valid ? 'verified' : 'invalid')
              }
            />
          )}
          {saveError && (
            <p className="secret-row__save-error" role="alert">
              {saveError}
            </p>
          )}
          <div className="secret-row__edit-actions">
            <button
              type="button"
              onClick={handleCancel}
              disabled={isSaving}
              className="secret-row__cancel-btn"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={handleSave}
              disabled={isSaving || !editValue || !!validationError}
              className="secret-row__save-btn"
            >
              {isSaving ? 'Saving…' : 'Save'}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
