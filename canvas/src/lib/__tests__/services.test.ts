import { describe, it, expect } from 'vitest';
import { SERVICES, SERVICE_GROUP_ORDER, getDefaultKeyName } from '../services';
import type { SecretGroup } from '@/types/secrets';

describe('services', () => {
  describe('SERVICES registry', () => {
    it('has all four service groups', () => {
      expect(SERVICES).toHaveProperty('github');
      expect(SERVICES).toHaveProperty('anthropic');
      expect(SERVICES).toHaveProperty('openrouter');
      expect(SERVICES).toHaveProperty('custom');
    });

    it('github config is correct', () => {
      const gh = SERVICES.github;
      expect(gh.label).toBe('GitHub');
      expect(gh.icon).toBe('github');
      expect(gh.keyNames).toContain('GITHUB_TOKEN');
      expect(gh.docsUrl).toContain('github.com');
      expect(gh.testSupported).toBe(true);
    });

    it('anthropic config is correct', () => {
      const ant = SERVICES.anthropic;
      expect(ant.label).toBe('Anthropic');
      expect(ant.keyNames).toContain('ANTHROPIC_API_KEY');
      expect(ant.testSupported).toBe(true);
    });

    it('openrouter config is correct', () => {
      const or = SERVICES.openrouter;
      expect(or.label).toBe('OpenRouter');
      expect(or.keyNames).toContain('OPENROUTER_API_KEY');
      expect(or.testSupported).toBe(true);
    });

    it('custom has no test support', () => {
      expect(SERVICES.custom.testSupported).toBe(false);
      expect(SERVICES.custom.keyNames).toEqual([]);
    });

    it('every service has a non-empty label and icon', () => {
      for (const key of Object.keys(SERVICES) as SecretGroup[]) {
        expect(SERVICES[key].label.length).toBeGreaterThan(0);
        expect(SERVICES[key].icon.length).toBeGreaterThan(0);
      }
    });
  });

  describe('SERVICE_GROUP_ORDER', () => {
    it('contains all four groups', () => {
      expect(SERVICE_GROUP_ORDER).toContain('github');
      expect(SERVICE_GROUP_ORDER).toContain('anthropic');
      expect(SERVICE_GROUP_ORDER).toContain('openrouter');
      expect(SERVICE_GROUP_ORDER).toContain('custom');
      expect(SERVICE_GROUP_ORDER).toHaveLength(4);
    });

    it('custom is last (catch-all at bottom)', () => {
      expect(SERVICE_GROUP_ORDER[SERVICE_GROUP_ORDER.length - 1]).toBe('custom');
    });
  });

  describe('getDefaultKeyName', () => {
    it('returns GITHUB_TOKEN for github', () => {
      expect(getDefaultKeyName('github')).toBe('GITHUB_TOKEN');
    });

    it('returns ANTHROPIC_API_KEY for anthropic', () => {
      expect(getDefaultKeyName('anthropic')).toBe('ANTHROPIC_API_KEY');
    });

    it('returns OPENROUTER_API_KEY for openrouter', () => {
      expect(getDefaultKeyName('openrouter')).toBe('OPENROUTER_API_KEY');
    });

    it('returns empty string for custom (no default)', () => {
      expect(getDefaultKeyName('custom')).toBe('');
    });
  });
});
