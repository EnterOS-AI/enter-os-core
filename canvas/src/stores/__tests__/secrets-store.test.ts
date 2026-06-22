import { describe, it, expect, beforeEach, vi } from 'vitest';

// ── Mock the secrets API before importing the store ──────────────

const mockListSecrets = vi.fn();
const mockCreateSecret = vi.fn();
const mockUpdateSecret = vi.fn();
const mockDeleteSecret = vi.fn();

vi.mock('@/lib/api/secrets', () => ({
  listSecrets: (...args: unknown[]) => mockListSecrets(...args),
  createSecret: (...args: unknown[]) => mockCreateSecret(...args),
  updateSecret: (...args: unknown[]) => mockUpdateSecret(...args),
  deleteSecret: (...args: unknown[]) => mockDeleteSecret(...args),
}));

vi.mock('@/lib/validation/secret-formats', () => ({
  inferGroup: (name: string) => {
    const upper = name.toUpperCase();
    if (upper.includes('GITHUB')) return 'github';
    if (upper.includes('ANTHROPIC')) return 'anthropic';
    if (upper.includes('OPENROUTER')) return 'openrouter';
    return 'custom';
  },
}));

vi.mock('@/lib/services', () => ({
  SERVICE_GROUP_ORDER: ['github', 'anthropic', 'openrouter', 'custom'] as const,
}));

import { useSecretsStore } from '../secrets-store';
import type { Secret } from '@/types/secrets';

// ── Helpers ──────────────────────────────────────────────────────

function makeSecret(overrides: Partial<Secret> & { name: string }): Secret {
  return {
    masked_value: '••••••xK9f',
    group: 'github',
    status: 'unverified',
    updated_at: '2026-04-10T00:00:00Z',
    ...overrides,
  };
}

const WS_ID = 'ws-test-123';

// ── Tests ────────────────────────────────────────────────────────

describe('secrets-store', () => {
  beforeEach(() => {
    // Reset store state
    useSecretsStore.setState({
      secrets: [],
      isLoading: false,
      error: null,
      isPanelOpen: false,
      editingKey: null,
      isAddFormOpen: false,
      searchQuery: '',
    });

    vi.clearAllMocks();
  });

  // ── Panel UI state ───────────────────────────────────────────

  describe('panel state', () => {
    it('opens and closes the panel', () => {
      const store = useSecretsStore.getState();
      expect(store.isPanelOpen).toBe(false);

      store.openPanel();
      expect(useSecretsStore.getState().isPanelOpen).toBe(true);

      store.closePanel();
      expect(useSecretsStore.getState().isPanelOpen).toBe(false);
    });

    it('openPanel with expandAddForm option', () => {
      useSecretsStore.getState().openPanel({ expandAddForm: true });
      expect(useSecretsStore.getState().isAddFormOpen).toBe(true);
      expect(useSecretsStore.getState().isPanelOpen).toBe(true);
    });

    it('closePanel resets all UI state', () => {
      useSecretsStore.setState({
        isPanelOpen: true,
        editingKey: 'MY_KEY',
        isAddFormOpen: true,
        searchQuery: 'test',
      });

      useSecretsStore.getState().closePanel();
      const s = useSecretsStore.getState();
      expect(s.isPanelOpen).toBe(false);
      expect(s.editingKey).toBeNull();
      expect(s.isAddFormOpen).toBe(false);
      expect(s.searchQuery).toBe('');
    });

    it('setEditingKey clears isAddFormOpen', () => {
      useSecretsStore.setState({ isAddFormOpen: true });
      useSecretsStore.getState().setEditingKey('MY_KEY');
      const s = useSecretsStore.getState();
      expect(s.editingKey).toBe('MY_KEY');
      expect(s.isAddFormOpen).toBe(false);
    });

    it('setAddFormOpen clears editingKey', () => {
      useSecretsStore.setState({ editingKey: 'MY_KEY' });
      useSecretsStore.getState().setAddFormOpen(true);
      const s = useSecretsStore.getState();
      expect(s.isAddFormOpen).toBe(true);
      expect(s.editingKey).toBeNull();
    });

    it('setSearchQuery updates search state', () => {
      useSecretsStore.getState().setSearchQuery('anthropic');
      expect(useSecretsStore.getState().searchQuery).toBe('anthropic');
    });
  });

  // ── Fetch secrets ────────────────────────────────────────────

  describe('fetchSecrets', () => {
    it('loads secrets successfully', async () => {
      const secrets = [
        makeSecret({ name: 'GITHUB_TOKEN' }),
        makeSecret({ name: 'ANTHROPIC_API_KEY', group: 'anthropic' }),
      ];
      mockListSecrets.mockResolvedValueOnce(secrets);

      await useSecretsStore.getState().fetchSecrets(WS_ID);

      expect(mockListSecrets).toHaveBeenCalledWith(WS_ID);
      const s = useSecretsStore.getState();
      expect(s.secrets).toEqual(secrets);
      expect(s.isLoading).toBe(false);
      expect(s.error).toBeNull();
    });

    it('sets loading state while fetching', async () => {
      let resolvePromise!: (val: Secret[]) => void;
      mockListSecrets.mockReturnValueOnce(
        new Promise<Secret[]>((resolve) => { resolvePromise = resolve; })
      );

      const fetchPromise = useSecretsStore.getState().fetchSecrets(WS_ID);
      expect(useSecretsStore.getState().isLoading).toBe(true);

      resolvePromise([]);
      await fetchPromise;
      expect(useSecretsStore.getState().isLoading).toBe(false);
    });

    it('handles fetch error gracefully', async () => {
      mockListSecrets.mockRejectedValueOnce(new Error('Network error'));

      await useSecretsStore.getState().fetchSecrets(WS_ID);

      const s = useSecretsStore.getState();
      expect(s.isLoading).toBe(false);
      expect(s.error).toBe('Network error');
      expect(s.secrets).toEqual([]);
    });

    it('uses fallback error message for non-Error throws', async () => {
      mockListSecrets.mockRejectedValueOnce('something unexpected');

      await useSecretsStore.getState().fetchSecrets(WS_ID);

      expect(useSecretsStore.getState().error).toContain('Couldn\u2019t load');
    });
  });

  // ── Create secret ────────────────────────────────────────────

  describe('createSecret', () => {
    it('adds secret to store and closes add form', async () => {
      useSecretsStore.setState({ isAddFormOpen: true });
      const newSecret = makeSecret({ name: 'NEW_KEY', group: 'custom' });
      mockCreateSecret.mockResolvedValueOnce(newSecret);

      await useSecretsStore.getState().createSecret(WS_ID, 'NEW_KEY', 'val123');

      expect(mockCreateSecret).toHaveBeenCalledWith(WS_ID, 'NEW_KEY', 'val123');
      const s = useSecretsStore.getState();
      expect(s.secrets).toHaveLength(1);
      expect(s.secrets[0].name).toBe('NEW_KEY');
      expect(s.isAddFormOpen).toBe(false);
    });

    it('propagates errors on create failure', async () => {
      mockCreateSecret.mockRejectedValueOnce(new Error('409 Conflict'));

      await expect(
        useSecretsStore.getState().createSecret(WS_ID, 'KEY', 'val'),
      ).rejects.toThrow('409 Conflict');
    });
  });

  // ── Update secret ────────────────────────────────────────────

  describe('updateSecret', () => {
    it('replaces secret in-place and clears editingKey', async () => {
      const original = makeSecret({ name: 'GITHUB_TOKEN', status: 'verified' });
      const updated = makeSecret({ name: 'GITHUB_TOKEN', status: 'unverified', masked_value: '••••new' });
      useSecretsStore.setState({ secrets: [original], editingKey: 'GITHUB_TOKEN' });
      mockUpdateSecret.mockResolvedValueOnce(updated);

      await useSecretsStore.getState().updateSecret(WS_ID, 'GITHUB_TOKEN', 'newval');

      expect(mockUpdateSecret).toHaveBeenCalledWith(WS_ID, 'GITHUB_TOKEN', 'newval');
      const s = useSecretsStore.getState();
      expect(s.secrets[0].masked_value).toBe('••••new');
      expect(s.editingKey).toBeNull();
    });

    it('does not modify other secrets', async () => {
      const s1 = makeSecret({ name: 'KEY_A', group: 'custom' });
      const s2 = makeSecret({ name: 'KEY_B', group: 'custom' });
      const s2Updated = makeSecret({ name: 'KEY_B', group: 'custom', masked_value: '••••upd' });
      useSecretsStore.setState({ secrets: [s1, s2] });
      mockUpdateSecret.mockResolvedValueOnce(s2Updated);

      await useSecretsStore.getState().updateSecret(WS_ID, 'KEY_B', 'newval');

      const secrets = useSecretsStore.getState().secrets;
      expect(secrets[0]).toEqual(s1); // Unchanged
      expect(secrets[1].masked_value).toBe('••••upd');
    });
  });

  // ── Delete secret ────────────────────────────────────────────

  describe('deleteSecret', () => {
    it('removes secret from store', async () => {
      const s1 = makeSecret({ name: 'KEEP_ME' });
      const s2 = makeSecret({ name: 'DELETE_ME' });
      useSecretsStore.setState({ secrets: [s1, s2] });
      mockDeleteSecret.mockResolvedValueOnce(undefined);

      await useSecretsStore.getState().deleteSecret(WS_ID, 'DELETE_ME');

      expect(mockDeleteSecret).toHaveBeenCalledWith(WS_ID, 'DELETE_ME');
      const secrets = useSecretsStore.getState().secrets;
      expect(secrets).toHaveLength(1);
      expect(secrets[0].name).toBe('KEEP_ME');
    });

    it('propagates errors on delete failure', async () => {
      useSecretsStore.setState({ secrets: [makeSecret({ name: 'KEY' })] });
      mockDeleteSecret.mockRejectedValueOnce(new Error('500 Internal'));

      await expect(
        useSecretsStore.getState().deleteSecret(WS_ID, 'KEY'),
      ).rejects.toThrow('500 Internal');

      // Secret should still be in the store (optimistic update not used)
      expect(useSecretsStore.getState().secrets).toHaveLength(1);
    });
  });

  // ── setSecretStatus ──────────────────────────────────────────

  describe('setSecretStatus', () => {
    it('updates status for specific secret', () => {
      const secret = makeSecret({ name: 'KEY', status: 'unverified' });
      useSecretsStore.setState({ secrets: [secret] });

      useSecretsStore.getState().setSecretStatus('KEY', 'verified');

      expect(useSecretsStore.getState().secrets[0].status).toBe('verified');
    });

    it('does not affect other secrets', () => {
      const s1 = makeSecret({ name: 'A', status: 'unverified' });
      const s2 = makeSecret({ name: 'B', status: 'unverified' });
      useSecretsStore.setState({ secrets: [s1, s2] });

      useSecretsStore.getState().setSecretStatus('A', 'invalid');

      expect(useSecretsStore.getState().secrets[0].status).toBe('invalid');
      expect(useSecretsStore.getState().secrets[1].status).toBe('unverified');
    });
  });

  // ── getGrouped (derived) ─────────────────────────────────────

  describe('getGrouped', () => {
    it('groups secrets by their group field', () => {
      useSecretsStore.setState({
        secrets: [
          makeSecret({ name: 'GITHUB_TOKEN', group: 'github' }),
          makeSecret({ name: 'ANTHROPIC_API_KEY', group: 'anthropic' }),
          makeSecret({ name: 'OPENROUTER_API_KEY', group: 'openrouter' }),
          makeSecret({ name: 'MY_CUSTOM', group: 'custom' }),
        ],
      });

      const grouped = useSecretsStore.getState().getGrouped();
      expect(grouped.github).toHaveLength(1);
      expect(grouped.anthropic).toHaveLength(1);
      expect(grouped.openrouter).toHaveLength(1);
      expect(grouped.custom).toHaveLength(1);
    });

    it('returns empty arrays for groups with no secrets', () => {
      useSecretsStore.setState({ secrets: [] });
      const grouped = useSecretsStore.getState().getGrouped();

      expect(grouped.github).toEqual([]);
      expect(grouped.anthropic).toEqual([]);
      expect(grouped.openrouter).toEqual([]);
      expect(grouped.custom).toEqual([]);
    });

    it('filters secrets by search query (case-insensitive)', () => {
      useSecretsStore.setState({
        secrets: [
          makeSecret({ name: 'GITHUB_TOKEN', group: 'github' }),
          makeSecret({ name: 'ANTHROPIC_API_KEY', group: 'anthropic' }),
          makeSecret({ name: 'MY_CUSTOM_KEY', group: 'custom' }),
        ],
        searchQuery: 'github',
      });

      const grouped = useSecretsStore.getState().getGrouped();
      expect(grouped.github).toHaveLength(1);
      expect(grouped.anthropic).toHaveLength(0);
      expect(grouped.custom).toHaveLength(0);
    });

    it('search filters case-insensitively', () => {
      useSecretsStore.setState({
        secrets: [
          makeSecret({ name: 'ANTHROPIC_API_KEY', group: 'anthropic' }),
        ],
        searchQuery: 'ANTHROPIC',
      });

      const grouped = useSecretsStore.getState().getGrouped();
      expect(grouped.anthropic).toHaveLength(1);
    });

    it('returns all when search query is empty', () => {
      useSecretsStore.setState({
        secrets: [
          makeSecret({ name: 'A', group: 'github' }),
          makeSecret({ name: 'B', group: 'custom' }),
        ],
        searchQuery: '',
      });

      const grouped = useSecretsStore.getState().getGrouped();
      expect(grouped.github).toHaveLength(1);
      expect(grouped.custom).toHaveLength(1);
    });
  });
});
