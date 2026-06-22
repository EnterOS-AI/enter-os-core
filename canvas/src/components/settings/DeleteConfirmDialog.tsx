'use client';
import { useState, useCallback, useEffect, useRef } from 'react';
import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { useSecretsStore } from '@/stores/secrets-store';
import { fetchDependents } from '@/lib/api/secrets';

const CONFIRM_DELAY_MS = 1_000;

interface DeleteConfirmDialogProps {
  workspaceId: string;
}

/**
 * Destructive confirmation dialog for deleting a secret key.
 *
 * Per spec §3.5 & §4.5:
 * - Shows dependent agents (fetched live on open)
 * - "Delete key" button disabled for 1s to prevent accidental double-click
 * - Red/destructive styling
 * - Focus-trapped (AlertDialog)
 */
export function DeleteConfirmDialog({ workspaceId }: DeleteConfirmDialogProps) {
  const [secretName, setSecretName] = useState<string | null>(null);
  const [dependents, setDependents] = useState<string[]>([]);
  const [isLoadingDependents, setIsLoadingDependents] = useState(false);
  const [isDeleting, setIsDeleting] = useState(false);
  const [confirmEnabled, setConfirmEnabled] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const deleteSecret = useSecretsStore((s) => s.deleteSecret);
  const confirmTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  const abortRef = useRef<AbortController | null>(null);

  // Clean up timer + abort fetch on unmount
  useEffect(() => {
    return () => {
      clearTimeout(confirmTimerRef.current);
      abortRef.current?.abort();
    };
  }, []);

  // Listen for delete requests from SecretRow
  useEffect(() => {
    function handler(e: Event) {
      const name = (e as CustomEvent<string>).detail;
      setSecretName(name);
      setConfirmEnabled(false);
      setDeleteError(null);
      setDependents([]);

      // Fetch dependents (cancel previous if any)
      if (abortRef.current) abortRef.current.abort();
      const controller = new AbortController();
      abortRef.current = controller;
      setIsLoadingDependents(true);
      fetchDependents(workspaceId, name)
        .then((deps) => { if (!controller.signal.aborted) setDependents(deps); })
        .catch(() => { if (!controller.signal.aborted) setDependents([]); })
        .finally(() => { if (!controller.signal.aborted) setIsLoadingDependents(false); });

      // Enable confirm after 1s delay
      clearTimeout(confirmTimerRef.current);
      confirmTimerRef.current = setTimeout(() => setConfirmEnabled(true), CONFIRM_DELAY_MS);
    }
    window.addEventListener('secret:delete-request', handler);
    return () => window.removeEventListener('secret:delete-request', handler);
  }, [workspaceId]);

  const handleDelete = useCallback(async () => {
    if (!secretName) return;
    setIsDeleting(true);
    setDeleteError(null);
    try {
      await deleteSecret(workspaceId, secretName);
      setSecretName(null);
    } catch (e) {
      setDeleteError(
        e instanceof Error ? e.message : 'Failed to delete key. Try again.',
      );
    } finally {
      setIsDeleting(false);
    }
  }, [secretName, deleteSecret, workspaceId]);

  const handleCancel = useCallback(() => {
    setSecretName(null);
    setDeleteError(null);
  }, []);

  return (
    <AlertDialog.Root
      open={secretName !== null}
      onOpenChange={(open) => { if (!open) handleCancel(); }}
    >
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="delete-dialog__overlay" />
        <AlertDialog.Content className="delete-dialog">
          <AlertDialog.Title className="delete-dialog__title">
            Delete &ldquo;{secretName}&rdquo;?
          </AlertDialog.Title>

          <AlertDialog.Description className="delete-dialog__desc">
            This key will be permanently removed.
            {isLoadingDependents && ' Checking for dependent agents…'}
          </AlertDialog.Description>

          {!isLoadingDependents && dependents.length > 0 && (
            <div className="delete-dialog__dependents">
              <p>Agents that depend on it may stop working:</p>
              <ul>
                {dependents.map((d) => (
                  <li key={d}>{d}</li>
                ))}
              </ul>
            </div>
          )}

          {!isLoadingDependents && dependents.length === 0 && (
            <p className="delete-dialog__no-dependents">
              No agents currently use this key.
            </p>
          )}

          <p className="delete-dialog__warning">This cannot be undone.</p>

          {deleteError && (
            <p className="delete-dialog__error" role="alert">
              {deleteError}
            </p>
          )}

          <div className="delete-dialog__actions">
            <AlertDialog.Cancel asChild>
              <button className="delete-dialog__cancel-btn" disabled={isDeleting}>
                Cancel
              </button>
            </AlertDialog.Cancel>
            <AlertDialog.Action asChild>
              <button
                className="delete-dialog__confirm-btn"
                onClick={handleDelete}
                disabled={!confirmEnabled || isDeleting}
              >
                {isDeleting ? 'Deleting…' : 'Delete key'}
              </button>
            </AlertDialog.Action>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}
