'use client';

import * as AlertDialog from '@radix-ui/react-alert-dialog';

interface UnsavedChangesGuardProps {
  open: boolean;
  onKeepEditing: () => void;
  onDiscard: () => void;
}

/**
 * "Discard unsaved changes?" guard dialog.
 *
 * Per spec §4.4:
 * - Shown when closing panel while a form has unsaved input
 * - NOT shown if the form is empty (opened but nothing typed)
 * - Focus-trapped (AlertDialog)
 */
export function UnsavedChangesGuard({
  open,
  onKeepEditing,
  onDiscard,
}: UnsavedChangesGuardProps) {
  return (
    <AlertDialog.Root open={open} onOpenChange={(o) => { if (!o) onKeepEditing(); }}>
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="guard-dialog__overlay" />
        <AlertDialog.Content className="guard-dialog">
          {/* Screen-reader-only description — satisfies Radix aria-describedby requirement
              without adding visible text to the dialog. */}
          <AlertDialog.Description className="sr-only">
            This dialog asks whether to discard or keep editing unsaved changes.
          </AlertDialog.Description>
          <AlertDialog.Title className="guard-dialog__title">
            Discard unsaved changes?
          </AlertDialog.Title>
          <div className="guard-dialog__actions">
            <AlertDialog.Cancel asChild>
              <button type="button" className="guard-dialog__keep-btn">
                Keep editing
              </button>
            </AlertDialog.Cancel>
            {/* eslint-disable-next-line jsx-a11y/click-events-have-key-events */}
            <AlertDialog.Action asChild>
              <button
                type="button"
                className="guard-dialog__discard-btn"
                onClick={(e) => { e.stopPropagation(); onDiscard(); }}
              >
                Discard
              </button>
            </AlertDialog.Action>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}
