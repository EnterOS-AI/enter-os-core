import { create } from 'zustand';
import type { Secret, SecretGroup, SecretStatus } from '@/types/secrets';
import * as api from '@/lib/api/secrets';
import { inferGroup } from '@/lib/validation/secret-formats';
import { SERVICE_GROUP_ORDER } from '@/lib/services';
import { markAllWorkspacesNeedRestart, markWorkspaceNeedsRestart } from '@/lib/canvas-actions';

// In api/secrets.ts, workspaceId === "global" routes to /settings/secrets;
// any other value is a workspace-scoped secret at /workspaces/:id/secrets.
const GLOBAL_WORKSPACE_ID = 'global';

// Global secrets affect all workspaces; workspace-scoped secrets only affect one.
function markAffectedWorkspaces(workspaceId: string): void {
  if (workspaceId === GLOBAL_WORKSPACE_ID) {
    markAllWorkspacesNeedRestart();
  } else {
    markWorkspaceNeedsRestart(workspaceId);
  }
}

export interface SecretsState {
  // --- data ---
  secrets: Secret[];
  isLoading: boolean;
  error: string | null;

  // --- ui ---
  isPanelOpen: boolean;
  editingKey: string | null;
  isAddFormOpen: boolean;
  searchQuery: string;

  // --- actions ---
  fetchSecrets: (workspaceId: string) => Promise<void>;
  createSecret: (
    workspaceId: string,
    name: string,
    value: string,
  ) => Promise<void>;
  updateSecret: (
    workspaceId: string,
    name: string,
    value: string,
  ) => Promise<void>;
  deleteSecret: (workspaceId: string, name: string) => Promise<void>;
  setSecretStatus: (name: string, status: SecretStatus) => void;

  openPanel: (opts?: {
    tab?: string;
    highlightService?: string;
    expandAddForm?: boolean;
  }) => void;
  closePanel: () => void;
  setEditingKey: (name: string | null) => void;
  setAddFormOpen: (open: boolean) => void;
  setSearchQuery: (q: string) => void;

  // --- derived ---
  getGrouped: () => Record<SecretGroup, Secret[]>;
}

export const useSecretsStore = create<SecretsState>((set, get) => ({
  secrets: [],
  isLoading: false,
  error: null,

  isPanelOpen: false,
  editingKey: null,
  isAddFormOpen: false,
  searchQuery: '',

  // ── data actions ────────────────────────────────────────────

  fetchSecrets: async (workspaceId) => {
    set({ isLoading: true, error: null });
    try {
      const secrets = await api.listSecrets(workspaceId);
      set({ secrets, isLoading: false });
    } catch (e) {
      set({
        isLoading: false,
        error:
          e instanceof Error
            ? e.message
            : 'Couldn\u2019t load your API keys. Refresh to try again.',
      });
    }
  },

  createSecret: async (workspaceId, name, value) => {
    const created = await api.createSecret(workspaceId, name, value);
    set((s) => ({ secrets: [...s.secrets, created], isAddFormOpen: false }));
    markAffectedWorkspaces(workspaceId);
  },

  updateSecret: async (workspaceId, name, value) => {
    const updated = await api.updateSecret(workspaceId, name, value);
    set((s) => ({
      secrets: s.secrets.map((sec) => (sec.name === name ? updated : sec)),
      editingKey: null,
    }));
    markAffectedWorkspaces(workspaceId);
  },

  deleteSecret: async (workspaceId, name) => {
    await api.deleteSecret(workspaceId, name);
    set((s) => ({
      secrets: s.secrets.filter((sec) => sec.name !== name),
    }));
    markAffectedWorkspaces(workspaceId);
  },

  setSecretStatus: (name, status) => {
    set((s) => ({
      secrets: s.secrets.map((sec) =>
        sec.name === name ? { ...sec, status } : sec,
      ),
    }));
  },

  // ── ui actions ──────────────────────────────────────────────

  openPanel: (opts) => {
    set({
      isPanelOpen: true,
      isAddFormOpen: opts?.expandAddForm ?? false,
    });
  },

  closePanel: () => {
    set({
      isPanelOpen: false,
      editingKey: null,
      isAddFormOpen: false,
      searchQuery: '',
    });
  },

  setEditingKey: (name) => set({ editingKey: name, isAddFormOpen: false }),
  setAddFormOpen: (open) => set({ isAddFormOpen: open, editingKey: null }),
  setSearchQuery: (q) => set({ searchQuery: q }),

  // ── derived ─────────────────────────────────────────────────

  getGrouped: () => {
    const { secrets, searchQuery } = get();
    const q = searchQuery.toLowerCase();

    const filtered = q
      ? secrets.filter((s) => s.name.toLowerCase().includes(q))
      : secrets;

    const grouped = Object.fromEntries(
      SERVICE_GROUP_ORDER.map((g) => [g, [] as Secret[]]),
    ) as Record<SecretGroup, Secret[]>;

    for (const secret of filtered) {
      const group = secret.group ?? inferGroup(secret.name);
      grouped[group].push(secret);
    }
    return grouped;
  },
}));
