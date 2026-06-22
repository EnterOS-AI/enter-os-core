'use client';

import { forwardRef } from 'react';
import { useSecretsStore } from '@/stores/secrets-store';
import * as Tooltip from '@radix-ui/react-tooltip';

/**
 * Gear icon button for the top bar. Toggles the SettingsPanel.
 *
 * Per spec §1.1:
 * - Position: right cluster of top bar, between bell and avatar
 * - Icon: 20×20 gear/cog
 * - Tooltip: "Settings ⌘," (300ms delay)
 * - Active state: accent fill when panel is open
 */
export const SettingsButton = forwardRef<HTMLButtonElement>(
  function SettingsButton(_props, ref) {
    const isPanelOpen = useSecretsStore((s) => s.isPanelOpen);
    const openPanel = useSecretsStore((s) => s.openPanel);
    const closePanel = useSecretsStore((s) => s.closePanel);
    const isMac = typeof navigator !== 'undefined' && /Mac/.test(navigator.userAgent);

    const handleClick = () => {
      if (isPanelOpen) closePanel();
      else openPanel();
    };

    return (
      <Tooltip.Provider delayDuration={300}>
        <Tooltip.Root>
          <Tooltip.Trigger asChild>
            <button
              ref={ref}
              onClick={handleClick}
              className={`settings-button ${isPanelOpen ? 'settings-button--active' : ''}`}
              aria-label="Settings"
              aria-expanded={isPanelOpen}
            >
              <GearIcon />
            </button>
          </Tooltip.Trigger>
          <Tooltip.Portal>
            <Tooltip.Content className="settings-button__tooltip" sideOffset={5}>
              Settings {isMac ? '⌘,' : 'Ctrl+,'}
              <Tooltip.Arrow />
            </Tooltip.Content>
          </Tooltip.Portal>
        </Tooltip.Root>
      </Tooltip.Provider>
    );
  },
);

function GearIcon() {
  return (
    <svg
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  );
}
