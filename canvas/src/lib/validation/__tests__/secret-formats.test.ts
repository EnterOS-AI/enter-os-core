import { describe, it, expect } from 'vitest';
import {
  validateSecretValue,
  isValidKeyName,
  inferGroup,
  maskSecretValue,
  SECRET_FORMAT_REGEXES,
} from '../secret-formats';

// ── validateSecretValue ──────────────────────────────────────────

describe('validateSecretValue', () => {
  describe('github', () => {
    it('accepts valid ghp_ prefixed tokens', () => {
      const token = 'ghp_' + 'a'.repeat(36);
      expect(validateSecretValue(token, 'github')).toBeNull();
    });

    it('accepts valid github_pat_ prefixed tokens', () => {
      const token = 'github_pat_' + 'A1B2C3D4E5F6G7H8I9J0K1L2M3N4O5P6Q7R8S9T0';
      expect(validateSecretValue(token, 'github')).toBeNull();
    });

    it('rejects tokens without correct prefix', () => {
      const result = validateSecretValue('invalid_token', 'github');
      expect(result).not.toBeNull();
      expect(result).toContain('ghp_');
    });

    it('rejects tokens too short', () => {
      const result = validateSecretValue('ghp_short', 'github');
      expect(result).not.toBeNull();
    });
  });

  describe('anthropic', () => {
    it('accepts valid sk-ant- prefixed keys', () => {
      const key = 'sk-ant-' + 'a'.repeat(90);
      expect(validateSecretValue(key, 'anthropic')).toBeNull();
    });

    it('rejects keys without correct prefix', () => {
      const result = validateSecretValue('wrong-prefix-key', 'anthropic');
      expect(result).not.toBeNull();
      expect(result).toContain('sk-ant-');
    });

    it('rejects keys too short', () => {
      const result = validateSecretValue('sk-ant-tooshort', 'anthropic');
      expect(result).not.toBeNull();
    });
  });

  describe('openrouter', () => {
    it('accepts valid sk-or- prefixed keys', () => {
      const key = 'sk-or-' + 'a'.repeat(40);
      expect(validateSecretValue(key, 'openrouter')).toBeNull();
    });

    it('rejects keys without correct prefix', () => {
      const result = validateSecretValue('no-prefix', 'openrouter');
      expect(result).not.toBeNull();
      expect(result).toContain('sk-or-');
    });
  });

  describe('custom', () => {
    it('accepts any non-empty string', () => {
      expect(validateSecretValue('anything', 'custom')).toBeNull();
    });

    it('rejects empty string', () => {
      const result = validateSecretValue('', 'custom');
      expect(result).not.toBeNull();
    });
  });
});

// ── isValidKeyName ───────────────────────────────────────────────

describe('isValidKeyName', () => {
  it('accepts UPPER_SNAKE_CASE names', () => {
    expect(isValidKeyName('GITHUB_TOKEN')).toBe(true);
    expect(isValidKeyName('MY_API_KEY_2')).toBe(true);
    expect(isValidKeyName('X')).toBe(true);
    expect(isValidKeyName('API123')).toBe(true);
  });

  it('rejects lowercase names', () => {
    expect(isValidKeyName('github_token')).toBe(false);
    expect(isValidKeyName('myKey')).toBe(false);
  });

  it('rejects names starting with number', () => {
    expect(isValidKeyName('1_KEY')).toBe(false);
  });

  it('rejects names starting with underscore', () => {
    expect(isValidKeyName('_KEY')).toBe(false);
  });

  it('rejects empty string', () => {
    expect(isValidKeyName('')).toBe(false);
  });

  it('rejects names with spaces', () => {
    expect(isValidKeyName('MY KEY')).toBe(false);
  });

  it('rejects names with dashes', () => {
    expect(isValidKeyName('MY-KEY')).toBe(false);
  });
});

// ── inferGroup ───────────────────────────────────────────────────

describe('inferGroup', () => {
  it('detects github from key name', () => {
    expect(inferGroup('GITHUB_TOKEN')).toBe('github');
    expect(inferGroup('my_github_pat')).toBe('github');
  });

  it('detects anthropic from key name', () => {
    expect(inferGroup('ANTHROPIC_API_KEY')).toBe('anthropic');
  });

  it('detects openrouter from key name', () => {
    expect(inferGroup('OPENROUTER_API_KEY')).toBe('openrouter');
  });

  it('falls back to custom for unknown names', () => {
    expect(inferGroup('MY_SECRET_KEY')).toBe('custom');
    expect(inferGroup('DATABASE_URL')).toBe('custom');
  });

  it('is case-insensitive', () => {
    expect(inferGroup('github_token')).toBe('github');
    expect(inferGroup('Github_Token')).toBe('github');
  });
});

// ── maskSecretValue ──────────────────────────────────────────────

describe('maskSecretValue', () => {
  it('masks ghp_ prefixed values showing prefix and last 4', () => {
    // Built via concatenation, not as a literal continuous string —
    // a literal `ghp_` + 36+ alphanumerics matches the secret-scan
    // workflow's own regex and false-positives merge_group / push runs.
    const value = 'ghp_' + 'x'.repeat(40);
    const masked = maskSecretValue(value);
    expect(masked.startsWith('ghp_')).toBe(true);
    expect(masked.endsWith(value.slice(-4))).toBe(true);
    expect(masked).toContain('•');
  });

  it('masks github_pat_ prefixed values', () => {
    const value = 'github_pat_' + 'x'.repeat(82);
    const masked = maskSecretValue(value);
    expect(masked.startsWith('github_pat_')).toBe(true);
    expect(masked.endsWith(value.slice(-4))).toBe(true);
  });

  it('masks sk-ant- prefixed values', () => {
    const value = 'sk-ant-' + 'x'.repeat(90);
    const masked = maskSecretValue(value);
    expect(masked.startsWith('sk-ant-')).toBe(true);
    expect(masked.endsWith(value.slice(-4))).toBe(true);
  });

  it('masks sk-or- prefixed values', () => {
    const value = 'sk-or-' + 'x'.repeat(40);
    const masked = maskSecretValue(value);
    expect(masked.startsWith('sk-or-')).toBe(true);
    expect(masked.endsWith(value.slice(-4))).toBe(true);
  });

  it('masks unknown format showing dots + last 4', () => {
    const value = 'somesecretvalue123';
    const masked = maskSecretValue(value);
    expect(masked.endsWith('e123')).toBe(true);
    expect(masked).toContain('•');
    expect(masked.startsWith('•')).toBe(true);
  });

  it('handles very short values gracefully', () => {
    const masked = maskSecretValue('abc');
    // Should have at least some dots before last 4 (or fewer) chars
    expect(masked).toContain('•');
  });
});

// ── Regex patterns ───────────────────────────────────────────────

describe('SECRET_FORMAT_REGEXES', () => {
  it('has entries for all known groups', () => {
    expect(SECRET_FORMAT_REGEXES).toHaveProperty('github');
    expect(SECRET_FORMAT_REGEXES).toHaveProperty('anthropic');
    expect(SECRET_FORMAT_REGEXES).toHaveProperty('openrouter');
    expect(SECRET_FORMAT_REGEXES).toHaveProperty('custom');
  });

  it('github regex allows both ghp_ and github_pat_ prefixes', () => {
    const regex = SECRET_FORMAT_REGEXES.github;
    expect(regex.test('ghp_' + 'a'.repeat(36))).toBe(true);
    expect(regex.test('github_pat_' + 'a'.repeat(36))).toBe(true);
  });
});
