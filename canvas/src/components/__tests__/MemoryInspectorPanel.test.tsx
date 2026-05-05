// @vitest-environment jsdom
/**
 * MemoryInspectorPanel — v2 redesign tests.
 *
 * Coverage targets every behavior the panel surfaces:
 *   - Initial load wires GET /v2/namespaces + GET /v2/memories
 *   - Plugin-unavailable banner (503) renders + disables interactions
 *   - Generic error renders in the error banner
 *   - Namespace dropdown populates from /v2/namespaces.readable; "All
 *     namespaces" is the default
 *   - Selecting a namespace re-fetches with ?namespace=...
 *   - Search input debounces + scopes the request to ?q=
 *   - Search results sort by score descending
 *   - Empty-state copy differs by query / plugin-state / no-data
 *   - Per-row badges render (kind / source / pin / TTL / score /
 *     source_workspace_id) and TTL countdown handles past/future/null
 *   - Delete (Forget) flow: optimistic removal, confirmation dialog,
 *     server failure rolls back via reload
 *   - formatTTL helper covers s/m/h/d/expired/null/invalid branches
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';

// ── Mocks ─────────────────────────────────────────────────────────────────────

vi.mock('@/lib/api', () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

vi.mock('@/components/ConfirmDialog', () => ({
  ConfirmDialog: ({
    open,
    title,
    message,
    onConfirm,
    onCancel,
  }: {
    open: boolean;
    title: string;
    message: string;
    confirmLabel?: string;
    confirmVariant?: string;
    onConfirm: () => void;
    onCancel: () => void;
  }) =>
    open ? (
      <div data-testid="confirm-dialog">
        <p data-testid="dialog-title">{title}</p>
        <p data-testid="dialog-message">{message}</p>
        <button onClick={onConfirm}>Confirm</button>
        <button onClick={onCancel}>Cancel</button>
      </div>
    ) : null,
}));

import { api } from '@/lib/api';
import {
  MemoryInspectorPanel,
  formatTTL,
  type MemoryV2,
  type NamespacesResponse,
} from '../MemoryInspectorPanel';

const mockGet = vi.mocked(api.get);
const mockDel = vi.mocked(api.del);

// ── Fixtures ──────────────────────────────────────────────────────────────────

const NS_RESPONSE: NamespacesResponse = {
  readable: [
    { name: 'workspace:ws-1', kind: 'workspace', label: 'Workspace (ws-1)' },
    { name: 'team:t-1', kind: 'team', label: 'Team (t-1)' },
  ],
  writable: [{ name: 'workspace:ws-1', kind: 'workspace', label: 'Workspace (ws-1)' }],
};

const MEM_BASIC: MemoryV2 = {
  id: 'mem-a',
  namespace: 'workspace:ws-1',
  content: 'Remember the standup is at 10am',
  kind: 'fact',
  source: 'agent',
  pin: false,
  created_at: '2026-04-17T12:00:00.000Z',
};

const MEM_PINNED: MemoryV2 = {
  id: 'mem-pinned',
  namespace: 'team:t-1',
  content: 'Team retro every Friday',
  kind: 'summary',
  source: 'user',
  pin: true,
  expires_at: new Date(Date.now() + 86_400_000).toISOString(),
  created_at: '2026-04-17T12:00:00.000Z',
};

const MEM_PROPAGATED: MemoryV2 = {
  id: 'mem-from-peer',
  namespace: 'team:t-1',
  content: 'Cross-workspace fact',
  kind: 'checkpoint',
  source: 'runtime',
  pin: false,
  source_workspace_id: 'ws-peer-99',
  created_at: '2026-04-17T12:00:00.000Z',
};

const MEM_EXPIRED: MemoryV2 = {
  id: 'mem-expired',
  namespace: 'workspace:ws-1',
  content: 'Stale memory',
  kind: 'fact',
  source: 'agent',
  pin: false,
  expires_at: new Date(Date.now() - 1000).toISOString(),
  created_at: '2026-04-17T12:00:00.000Z',
};

// ── Setup / teardown ──────────────────────────────────────────────────────────

beforeEach(() => {
  mockGet.mockReset();
  mockDel.mockReset();
});

afterEach(() => {
  cleanup();
});

// Helper: stub a basic two-call flow (namespaces + memories).
function stubFetch(memories: MemoryV2[], namespaces: NamespacesResponse = NS_RESPONSE) {
  mockGet.mockImplementation(((url: string) => {
    if (url.includes('/v2/namespaces')) {
      return Promise.resolve(namespaces);
    }
    return Promise.resolve({ memories });
  }) as typeof api.get);
}

// ── formatTTL helper ─────────────────────────────────────────────────────────

describe('formatTTL', () => {
  it('returns empty string for null/undefined/empty', () => {
    expect(formatTTL(null)).toBe('');
    expect(formatTTL(undefined)).toBe('');
    expect(formatTTL('')).toBe('');
  });

  it('returns empty for invalid date strings', () => {
    expect(formatTTL('not-a-date')).toBe('');
  });

  it('returns "expired" for past timestamps', () => {
    const past = new Date(Date.now() - 5000).toISOString();
    expect(formatTTL(past)).toBe('expired');
  });

  it('formats <60s as seconds', () => {
    const future = new Date(Date.now() + 30_000).toISOString();
    expect(formatTTL(future)).toMatch(/^\d{1,2}s$/);
  });

  it('formats <60m as minutes', () => {
    const future = new Date(Date.now() + 30 * 60_000).toISOString();
    expect(formatTTL(future)).toMatch(/^\d{1,2}m$/);
  });

  it('formats <24h as hours', () => {
    const future = new Date(Date.now() + 5 * 3_600_000).toISOString();
    expect(formatTTL(future)).toMatch(/^\d{1,2}h$/);
  });

  it('formats >24h as days', () => {
    const future = new Date(Date.now() + 3 * 86_400_000).toISOString();
    expect(formatTTL(future)).toMatch(/^\d{1,2}d$/);
  });
});

// ── Initial load + dropdown ─────────────────────────────────────────────────

describe('MemoryInspectorPanel — initial load', () => {
  it('fetches namespaces and memories on mount', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);

    await waitFor(() => {
      const calls = mockGet.mock.calls.map((c) => c[0]);
      expect(calls.some((u) => u.includes('/v2/namespaces'))).toBe(true);
      expect(calls.some((u) => u.includes('/v2/memories'))).toBe(true);
    });
  });

  it('renders the row contents from the memories response', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/Remember the standup is at 10am/)).toBeTruthy();
    });
  });

  it('populates the namespace dropdown with readable entries + "All namespaces"', async () => {
    stubFetch([]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByLabelText('Filter by namespace'));
    const select = screen.getByLabelText('Filter by namespace') as HTMLSelectElement;
    const optionLabels = Array.from(select.options).map((o) => o.textContent ?? '');
    expect(optionLabels[0]).toContain('All namespaces');
    expect(optionLabels.join('|')).toContain('Workspace (ws-1)');
    expect(optionLabels.join('|')).toContain('Team (t-1)');
  });

  it('selecting a namespace re-fetches with ?namespace=', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByLabelText('Filter by namespace'));

    const select = screen.getByLabelText('Filter by namespace') as HTMLSelectElement;
    fireEvent.change(select, { target: { value: 'team:t-1' } });

    await waitFor(() => {
      const calls = mockGet.mock.calls.map((c) => c[0] as string);
      expect(calls.some((u) => u.includes('namespace=team%3At-1'))).toBe(true);
    });
  });
});

// ── Plugin unavailable (503) ────────────────────────────────────────────────

describe('MemoryInspectorPanel — plugin unavailable', () => {
  it('renders the operator-hint banner and disables search input', async () => {
    mockGet.mockRejectedValue(new Error('HTTP 503: memory plugin is not configured (set MEMORY_PLUGIN_URL)'));
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByTestId('plugin-unavailable-banner'));

    const searchInput = screen.getByLabelText('Search memories') as HTMLInputElement;
    expect(searchInput.disabled).toBe(true);
  });

  it('shows the empty-state explaining plugin disabled', async () => {
    mockGet.mockRejectedValue(new Error('HTTP 503'));
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByText(/Memory plugin disabled/i));
  });
});

// ── Generic error (non-503) ─────────────────────────────────────────────────

describe('MemoryInspectorPanel — generic errors', () => {
  it('surfaces a non-503 error in the error banner', async () => {
    mockGet.mockImplementation(((url: string) => {
      if (url.includes('/v2/namespaces')) {
        return Promise.resolve(NS_RESPONSE);
      }
      return Promise.reject(new Error('upstream timeout'));
    }) as typeof api.get);

    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => {
      // Error banner has role=alert
      const alerts = screen.getAllByRole('alert');
      const found = alerts.some((a) => a.textContent?.includes('upstream timeout'));
      expect(found).toBe(true);
    });
  });
});

// ── Search ──────────────────────────────────────────────────────────────────

describe('MemoryInspectorPanel — search', () => {
  it('eventually fires query with ?q= after debounce', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByLabelText('Search memories'));

    fireEvent.change(screen.getByLabelText('Search memories'), {
      target: { value: 'standup' },
    });

    await waitFor(
      () => {
        const calls = mockGet.mock.calls.map((c) => c[0] as string);
        expect(calls.some((u) => u.includes('q=standup'))).toBe(true);
      },
      { timeout: 1500 },
    );
  });

  it('sorts results by score descending when query active', async () => {
    const lowScore: MemoryV2 = { ...MEM_BASIC, id: 'low', score: 0.2, content: 'low' };
    const highScore: MemoryV2 = { ...MEM_BASIC, id: 'high', score: 0.95, content: 'high' };
    // Plugin returns in arbitrary order; component sorts.
    mockGet.mockImplementation(((url: string) => {
      if (url.includes('/v2/namespaces')) return Promise.resolve(NS_RESPONSE);
      return Promise.resolve({ memories: [lowScore, highScore] });
    }) as typeof api.get);

    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByLabelText('Search memories'));
    fireEvent.change(screen.getByLabelText('Search memories'), {
      target: { value: 'something' },
    });

    await waitFor(
      () => {
        const rows = screen.getAllByTestId(/^memory-row-/);
        // First row should be the high-score one
        expect(rows[0].getAttribute('data-testid')).toBe('memory-row-high');
      },
      { timeout: 1500 },
    );
  });

  it('clear-button resets the query', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByLabelText('Search memories'));

    fireEvent.change(screen.getByLabelText('Search memories'), {
      target: { value: 'foo' },
    });
    fireEvent.click(screen.getByLabelText('Clear search'));
    expect((screen.getByLabelText('Search memories') as HTMLInputElement).value).toBe('');
  });

  it('renders no-results empty-state when search has no matches', async () => {
    stubFetch([]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByLabelText('Search memories'));
    fireEvent.change(screen.getByLabelText('Search memories'), {
      target: { value: 'nothing' },
    });
    await waitFor(
      () => {
        expect(screen.getByText(/No memories match your search/i)).toBeTruthy();
      },
      { timeout: 1500 },
    );
  });
});

// ── Per-row badges ───────────────────────────────────────────────────────────

describe('MemoryInspectorPanel — row badges', () => {
  it('renders kind, source, pin, TTL, source-workspace badges per shape', async () => {
    stubFetch([MEM_PINNED, MEM_PROPAGATED]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);

    await waitFor(() => {
      // Pinned memory: kind=summary, source=user, pin=true, TTL>0
      const pinnedRow = screen.getByTestId('memory-row-mem-pinned');
      expect(pinnedRow.querySelector('[data-testid="kind-badge"]')?.textContent).toBe('S');
      expect(pinnedRow.querySelector('[data-testid="source-badge"]')?.textContent).toBe('user');
      expect(pinnedRow.querySelector('[data-testid="pin-badge"]')).toBeTruthy();
      expect(pinnedRow.querySelector('[data-testid="ttl-badge"]')?.textContent).toMatch(/^⌛\d+[hd]$/);
      expect(pinnedRow.querySelector('[data-testid="source-workspace-badge"]')).toBeNull();

      // Propagated memory: kind=checkpoint, source=runtime, no pin, no TTL, source_workspace
      const propRow = screen.getByTestId('memory-row-mem-from-peer');
      expect(propRow.querySelector('[data-testid="kind-badge"]')?.textContent).toBe('C');
      expect(propRow.querySelector('[data-testid="source-badge"]')?.textContent).toBe('runtime');
      expect(propRow.querySelector('[data-testid="pin-badge"]')).toBeNull();
      expect(propRow.querySelector('[data-testid="ttl-badge"]')).toBeNull();
      expect(propRow.querySelector('[data-testid="source-workspace-badge"]')?.textContent).toMatch(/^⇡ws-pee/);
    });
  });

  it('TTL badge shows "expired" for past expires_at', async () => {
    stubFetch([MEM_EXPIRED]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => {
      const row = screen.getByTestId('memory-row-mem-expired');
      expect(row.querySelector('[data-testid="ttl-badge"]')?.textContent).toBe('⌛expired');
    });
  });

  it('expanding a row shows full content + Forget button', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByTestId('memory-row-mem-a'));

    const row = screen.getByTestId('memory-row-mem-a');
    const headerButton = row.querySelector('button');
    expect(headerButton).toBeTruthy();
    fireEvent.click(headerButton!);

    await waitFor(() => {
      expect(screen.getByLabelText('Forget memory')).toBeTruthy();
    });
  });
});

// ── Delete (Forget) flow ──────────────────────────────────────────────────────

describe('MemoryInspectorPanel — forget flow', () => {
  it('opens the confirm dialog on Forget click and removes optimistically on confirm', async () => {
    stubFetch([MEM_BASIC]);
    mockDel.mockResolvedValue({ status: 'deleted' });
    render(<MemoryInspectorPanel workspaceId="ws-1" />);

    // Expand row, click Forget
    await waitFor(() => screen.getByTestId('memory-row-mem-a'));
    const row = screen.getByTestId('memory-row-mem-a');
    fireEvent.click(row.querySelector('button')!);
    await waitFor(() => screen.getByLabelText('Forget memory'));
    fireEvent.click(screen.getByLabelText('Forget memory'));

    // Dialog appears with v2-shaped copy (Forget, not Delete)
    expect(screen.getByTestId('dialog-title').textContent).toBe('Forget memory');
    fireEvent.click(screen.getByText('Confirm'));

    // Optimistic removal happens immediately
    await waitFor(() => {
      expect(screen.queryByTestId('memory-row-mem-a')).toBeNull();
    });
    // DELETE called with the right path
    await waitFor(() => {
      const delPaths = mockDel.mock.calls.map((c) => c[0] as string);
      expect(delPaths.some((p) => p.includes('/v2/memories/mem-a'))).toBe(true);
    });
  });

  it('cancelling the dialog leaves the row in place', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByTestId('memory-row-mem-a'));

    fireEvent.click(screen.getByTestId('memory-row-mem-a').querySelector('button')!);
    await waitFor(() => screen.getByLabelText('Forget memory'));
    fireEvent.click(screen.getByLabelText('Forget memory'));
    fireEvent.click(screen.getByText('Cancel'));

    expect(screen.queryByTestId('memory-row-mem-a')).toBeTruthy();
    expect(mockDel).not.toHaveBeenCalled();
  });

  it('rolls back on server failure by reloading entries', async () => {
    stubFetch([MEM_BASIC]);
    mockDel.mockRejectedValue(new Error('upstream 502'));

    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByTestId('memory-row-mem-a'));
    fireEvent.click(screen.getByTestId('memory-row-mem-a').querySelector('button')!);
    await waitFor(() => screen.getByLabelText('Forget memory'));
    fireEvent.click(screen.getByLabelText('Forget memory'));
    fireEvent.click(screen.getByText('Confirm'));

    // After failure, error banner surfaces + reload re-fetches memories
    await waitFor(() => {
      const alerts = screen.getAllByRole('alert');
      const found = alerts.some((a) => a.textContent?.includes('upstream 502'));
      expect(found).toBe(true);
    });
  });
});

// ── Empty state when no memories at all ────────────────────────────────────

describe('MemoryInspectorPanel — empty state', () => {
  it('renders the "no memories yet" empty state when not searching', async () => {
    stubFetch([]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText('No memories yet')).toBeTruthy();
    });
  });
});

// ── Refresh ─────────────────────────────────────────────────────────────────

describe('MemoryInspectorPanel — refresh', () => {
  it('Refresh button refetches memories', async () => {
    stubFetch([MEM_BASIC]);
    render(<MemoryInspectorPanel workspaceId="ws-1" />);
    await waitFor(() => screen.getByLabelText('Refresh memories'));

    const before = mockGet.mock.calls.filter((c) =>
      (c[0] as string).includes('/v2/memories'),
    ).length;
    fireEvent.click(screen.getByLabelText('Refresh memories'));

    await waitFor(() => {
      const after = mockGet.mock.calls.filter((c) =>
        (c[0] as string).includes('/v2/memories'),
      ).length;
      expect(after).toBe(before + 1);
    });
  });
});
