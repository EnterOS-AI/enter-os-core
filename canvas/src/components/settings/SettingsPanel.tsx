'use client';

import { createRef, useCallback, useEffect, useState } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import { useSecretsStore } from '@/stores/secrets-store';
import { useKeyboardShortcut } from '@/hooks/use-keyboard-shortcut';
import { SettingsTabs } from './SettingsTabs';
import { UnsavedChangesGuard } from './UnsavedChangesGuard';

/** Module-level ref so TopBar's SettingsButton can receive focus back on close. */
export const settingsGearRef = createRef<HTMLButtonElement>();

interface SettingsPanelProps {
  workspaceId: string;
}

/**
 * Right-anchored slide-over drawer (480px) for workspace settings.
 *
 * Per UX spec:
 * - `aria-modal="false"` — canvas stays interactive behind the panel
 * - 200ms ease-out slide animation (respects prefers-reduced-motion)
 * - Backdrop: rgba(0,0,0,0.3), click to close (with unsaved guard)
 * - Tabs: "API Keys" (active) | "General" (disabled placeholder)
 * - Keyboard: Cmd+, / Ctrl+, toggles, Escape closes
 */
export function SettingsPanel({ workspaceId }: SettingsPanelProps) {
  const isPanelOpen = useSecretsStore((s) => s.isPanelOpen);
  const closePanel = useSecretsStore((s) => s.closePanel);
  const openPanel = useSecretsStore((s) => s.openPanel);
  const fetchSecrets = useSecretsStore((s) => s.fetchSecrets);
  const isAddFormOpen = useSecretsStore((s) => s.isAddFormOpen);
  const editingKey = useSecretsStore((s) => s.editingKey);

  const hasDirtyForm = isAddFormOpen || editingKey !== null;

  // Cmd+, / Ctrl+, toggle
  const isMac = typeof navigator !== 'undefined' && /Mac/.test(navigator.userAgent);
  const toggle = useCallback(() => {
    if (isPanelOpen) closePanel();
    else openPanel();
  }, [isPanelOpen, closePanel, openPanel]);
  useKeyboardShortcut(',', toggle, { meta: isMac, ctrl: !isMac });

  // Load secrets when panel opens
  useEffect(() => {
    if (isPanelOpen) {
      fetchSecrets(workspaceId);
    }
  }, [isPanelOpen, fetchSecrets, workspaceId]);

  // Guard: track whether we should show unsaved-changes dialog
  const [showGuard, setShowGuard] = useState(false);

  const handleOpenChange = useCallback(
    (open: boolean) => {
      if (!open && hasDirtyForm) {
        setShowGuard(true);
        return;
      }
      if (!open) {
        closePanel();
        settingsGearRef.current?.focus();
      }
    },
    [hasDirtyForm, closePanel],
  );

  const confirmDiscard = useCallback(() => {
    setShowGuard(false);
    closePanel();
    settingsGearRef.current?.focus();
  }, [closePanel]);

  return (
    <>
      <Dialog.Root open={isPanelOpen} onOpenChange={handleOpenChange} modal={false}>
        <Dialog.Portal>
          <Dialog.Overlay className="settings-panel__backdrop" />
          <Dialog.Content
            className="settings-panel"
            aria-label="Settings: API Keys"
            aria-describedby={undefined}
            onEscapeKeyDown={(e) => {
              if (hasDirtyForm) {
                e.preventDefault();
                setShowGuard(true);
              }
            }}
          >
            <div className="settings-panel__header">
              <Dialog.Title className="settings-panel__title">
                Settings
              </Dialog.Title>
              <Dialog.Close asChild>
                <button
                  className="settings-panel__close"
                  aria-label="Close settings"
                >
                  ×
                </button>
              </Dialog.Close>
            </div>

            <SettingsTabs workspaceId={workspaceId} />

            <div className="settings-panel__footer">
              <span className="settings-panel__shortcut-hint">
                {isMac ? '⌘,' : 'Ctrl+,'}
              </span>
              <span className="settings-panel__separator">·</span>
              <a
                href="https://doc.moleculesai.app/docs/tokens"
                target="_blank"
                rel="noopener noreferrer"
                className="settings-panel__docs-link"
              >
                Learn about secrets →
              </a>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      <UnsavedChangesGuard
        open={showGuard}
        onKeepEditing={() => setShowGuard(false)}
        onDiscard={confirmDiscard}
      />
    </>
  );
}

