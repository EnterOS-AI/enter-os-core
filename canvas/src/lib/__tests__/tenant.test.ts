/**
 * @vitest-environment jsdom
 */
import { describe, it, expect, vi, afterEach } from 'vitest';
import { getTenantSlug } from '../tenant';

afterEach(() => {
  vi.unstubAllGlobals();
});

// Shim window.location.hostname for each case.
function setHost(host: string) {
  Object.defineProperty(window, 'location', {
    value: { hostname: host },
    writable: true,
  });
}

describe('getTenantSlug', () => {
  it('returns slug for tenant subdomain', () => {
    setHost('acme.moleculesai.app');
    expect(getTenantSlug()).toBe('acme');
  });

  it('is case-insensitive', () => {
    setHost('ACME.MoleculesAI.app');
    expect(getTenantSlug()).toBe('acme');
  });

  it('returns empty for reserved subdomains', () => {
    for (const s of ['app', 'www', 'api', 'admin']) {
      setHost(`${s}.moleculesai.app`);
      expect(getTenantSlug()).toBe('');
    }
  });

  it('returns empty for non-SaaS hosts', () => {
    setHost('localhost');
    expect(getTenantSlug()).toBe('');
  });

  it('returns empty for vercel preview URL', () => {
    setHost('molecule-canvas-abc123.vercel.app');
    expect(getTenantSlug()).toBe('');
  });

  it('returns empty for apex', () => {
    setHost('moleculesai.app');
    // doesn't end with "." + suffix
    expect(getTenantSlug()).toBe('');
  });
});
