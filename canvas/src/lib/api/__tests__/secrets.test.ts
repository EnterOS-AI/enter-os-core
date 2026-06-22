import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';

// Set PLATFORM_URL before importing
vi.stubEnv('NEXT_PUBLIC_PLATFORM_URL', 'http://localhost:8080');

// Must import after env stub
const secretsModule = await import('../secrets');
const {
  listSecrets,
  createSecret,
  updateSecret,
  deleteSecret,
  validateSecret,
  fetchDependents,
  ApiError,
} = secretsModule;

// ── Helpers ──────────────────────────────────────────────────────

const WS_ID = 'ws-test-1';

function mockFetch(body: unknown, status = 200) {
  return vi.fn(() =>
    Promise.resolve({
      ok: status >= 200 && status < 300,
      status,
      statusText: status === 200 ? 'OK' : 'Error',
      json: () => Promise.resolve(body),
    } as Response),
  );
}

function mockFetch204() {
  return vi.fn(() =>
    Promise.resolve({
      ok: true,
      status: 204,
      statusText: 'No Content',
      json: () => Promise.reject(new Error('No body')),
    } as Response),
  );
}

// ── Tests ────────────────────────────────────────────────────────

describe('secrets API client', () => {
  const originalFetch = global.fetch;

  afterEach(() => {
    global.fetch = originalFetch;
  });

  describe('listSecrets', () => {
    it('sends GET to /workspaces/{id}/secrets and transforms response', async () => {
      // Platform returns a flat array: [{ key, has_value, scope, created_at, updated_at }]
      const platformResponse = [
        { key: 'GITHUB_TOKEN', has_value: true, scope: 'workspace', created_at: '2026-01-01', updated_at: '2026-01-02' },
      ];
      global.fetch = mockFetch(platformResponse);

      const result = await listSecrets(WS_ID);

      expect(global.fetch).toHaveBeenCalledWith(
        `http://localhost:8080/workspaces/${WS_ID}/secrets`,
        expect.objectContaining({
          headers: expect.objectContaining({
            'Content-Type': 'application/json',
          }),
        }),
      );
      expect(result).toEqual([
        {
          name: 'GITHUB_TOKEN',
          masked_value: '••••••••',
          group: 'github',
          status: 'unverified',
          updated_at: '2026-01-02',
        },
      ]);
    });

    it('returns empty array when response is not an array', async () => {
      global.fetch = mockFetch({ unexpected: 'data' });
      const result = await listSecrets(WS_ID);
      expect(result).toEqual([]);
    });

    it('routes global workspaceId to /settings/secrets', async () => {
      global.fetch = mockFetch([]);
      await listSecrets('global');
      expect(global.fetch).toHaveBeenCalledWith(
        'http://localhost:8080/settings/secrets',
        expect.anything(),
      );
    });

    it('throws ApiError on non-OK response', async () => {
      global.fetch = mockFetch({ message: 'Unauthorized' }, 401);

      await expect(listSecrets(WS_ID)).rejects.toThrow(ApiError);
      await expect(listSecrets(WS_ID)).rejects.toThrow('Unauthorized');
    });
  });

  describe('createSecret', () => {
    it('sends PUT with key and value', async () => {
      global.fetch = mockFetch({});

      const result = await createSecret(WS_ID, 'MY_KEY', 'mysecretval');

      expect(global.fetch).toHaveBeenCalledWith(
        `http://localhost:8080/workspaces/${WS_ID}/secrets`,
        expect.objectContaining({
          method: 'PUT',
          body: JSON.stringify({ key: 'MY_KEY', value: 'mysecretval' }),
        }),
      );
      expect(result.name).toBe('MY_KEY');
      expect(result.masked_value).toBe('••••••••');
      expect(result.status).toBe('unverified');
    });
  });

  describe('updateSecret', () => {
    it('sends PUT to base secrets URL with key and value', async () => {
      global.fetch = mockFetch({});

      const result = await updateSecret(WS_ID, 'MY_KEY', 'newval');

      expect(global.fetch).toHaveBeenCalledWith(
        `http://localhost:8080/workspaces/${WS_ID}/secrets`,
        expect.objectContaining({
          method: 'PUT',
          body: JSON.stringify({ key: 'MY_KEY', value: 'newval' }),
        }),
      );
      expect(result.name).toBe('MY_KEY');
      expect(result.status).toBe('unverified');
    });

    it('handles special characters in key names', async () => {
      global.fetch = mockFetch({});

      const result = await updateSecret(WS_ID, 'MY KEY/SPECIAL', 'val');

      expect(global.fetch).toHaveBeenCalledWith(
        `http://localhost:8080/workspaces/${WS_ID}/secrets`,
        expect.objectContaining({
          body: JSON.stringify({ key: 'MY KEY/SPECIAL', value: 'val' }),
        }),
      );
      expect(result.name).toBe('MY KEY/SPECIAL');
    });
  });

  describe('deleteSecret', () => {
    it('sends DELETE with encoded name and handles 204', async () => {
      global.fetch = mockFetch204();

      await expect(deleteSecret(WS_ID, 'KEY')).resolves.toBeUndefined();

      expect(global.fetch).toHaveBeenCalledWith(
        `http://localhost:8080/workspaces/${WS_ID}/secrets/KEY`,
        expect.objectContaining({ method: 'DELETE' }),
      );
    });
  });

  describe('validateSecret', () => {
    it('sends POST to /secrets/validate', async () => {
      global.fetch = mockFetch({ valid: true });

      const result = await validateSecret('github', 'ghp_abc');

      expect(global.fetch).toHaveBeenCalledWith(
        'http://localhost:8080/secrets/validate',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ provider: 'github', key: 'ghp_abc' }),
        }),
      );
      expect(result).toEqual({ valid: true });
    });

    it('returns error detail on invalid key', async () => {
      global.fetch = mockFetch({ valid: false, error: 'Key expired' });

      const result = await validateSecret('github', 'expired-key');
      expect(result.valid).toBe(false);
      expect(result.error).toBe('Key expired');
    });
  });

  describe('fetchDependents', () => {
    it('returns list of dependent workspace names', async () => {
      global.fetch = mockFetch({ dependents: ['ws-a', 'ws-b'] });

      const result = await fetchDependents(WS_ID, 'GITHUB_TOKEN');

      expect(global.fetch).toHaveBeenCalledWith(
        `http://localhost:8080/workspaces/${WS_ID}/secrets/GITHUB_TOKEN/dependents`,
        expect.anything(),
      );
      expect(result).toEqual(['ws-a', 'ws-b']);
    });

    it('returns empty array when endpoint does not exist (404)', async () => {
      global.fetch = mockFetch({ error: 'not found' }, 404);

      const result = await fetchDependents(WS_ID, 'GITHUB_TOKEN');
      expect(result).toEqual([]);
    });
  });

  describe('ApiError', () => {
    it('has status and message', () => {
      const err = new ApiError(404, 'Not found');
      expect(err.status).toBe(404);
      expect(err.message).toBe('Not found');
      expect(err.name).toBe('ApiError');
      expect(err instanceof Error).toBe(true);
    });
  });
});
