'use client';

import * as Tabs from '@radix-ui/react-tabs';
import { SecretsTab } from './SecretsTab';
import { TokensTab } from './TokensTab';
import { OrgTokensTab } from './OrgTokensTab';
import { OrgInfoTab } from './OrgInfoTab';

interface SettingsTabsProps {
  workspaceId: string;
}

/**
 * The tabbed body of the workspace settings surface — Secrets, Workspace
 * Tokens, Org API Keys, Organization.
 *
 * Extracted from SettingsPanel so the same content can render in two
 * places without duplication:
 *   1. The right-anchored slide-over drawer (the gear popover) — SettingsPanel.
 *   2. The concierge Settings view (embedded inline) — ConciergeShell.
 *
 * Pure presentation of the four tabs; all dirty-form / unsaved-guard /
 * keyboard-shortcut wiring stays in SettingsPanel where the popover owns it.
 */
export function SettingsTabs({ workspaceId }: SettingsTabsProps) {
  return (
    <Tabs.Root defaultValue="api-keys">
      <Tabs.List className="settings-panel__tabs" aria-label="Settings sections">
        <Tabs.Trigger value="api-keys" className="settings-panel__tab">
          Secrets
        </Tabs.Trigger>
        <Tabs.Trigger value="tokens" className="settings-panel__tab">
          Workspace Tokens
        </Tabs.Trigger>
        <Tabs.Trigger value="org-tokens" className="settings-panel__tab">
          Org API Keys
        </Tabs.Trigger>
        <Tabs.Trigger value="org-info" className="settings-panel__tab">
          Organization
        </Tabs.Trigger>
      </Tabs.List>

      <Tabs.Content value="api-keys" className="settings-panel__content">
        <SecretsTab workspaceId={workspaceId} />
      </Tabs.Content>

      <Tabs.Content value="tokens" className="settings-panel__content">
        <TokensTab workspaceId={workspaceId} />
      </Tabs.Content>

      <Tabs.Content value="org-tokens" className="settings-panel__content">
        <OrgTokensTab />
      </Tabs.Content>

      <Tabs.Content value="org-info" className="settings-panel__content">
        <OrgInfoTab />
      </Tabs.Content>
    </Tabs.Root>
  );
}
