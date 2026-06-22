import type { SecretGroup } from '@/types/secrets';

/** Regex patterns for client-side format validation per service. */
export const SECRET_FORMAT_REGEXES: Record<string, RegExp> = {
  github: /^(ghp_|github_pat_)[A-Za-z0-9_]{36,}$/,
  anthropic: /^sk-ant-[A-Za-z0-9\-_]{90,}$/,
  openrouter: /^sk-or-[A-Za-z0-9\-_]{40,}$/,
  custom: /^.{1,}$/,
};

/** Expected format hint shown on validation failure. */
export const SECRET_FORMAT_HINTS: Record<string, string> = {
  github: 'Expected format: ghp_ or github_pat_ prefix',
  anthropic: 'Expected format: sk-ant-...',
  openrouter: 'Expected format: sk-or-...',
  custom: 'Value cannot be empty',
};

/** Known key-name prefixes mapped to their masking display format. */
const PREFIX_PATTERNS: { prefix: string; group: SecretGroup }[] = [
  { prefix: 'github_pat_', group: 'github' },
  { prefix: 'ghp_', group: 'github' },
  { prefix: 'sk-ant-', group: 'anthropic' },
  { prefix: 'sk-or-', group: 'openrouter' },
];

/**
 * Mask a secret value showing a known prefix + dots + last 4 chars.
 * Examples:
 *   ghp_xxxx...xK9f  →  ghp_••••••••••••xK9f
 *   sk-ant-xxxx...Zq →  sk-ant-••••••••a3Zq
 *   unknown          →  ••••••••••••••9d2a
 */
export function maskSecretValue(value: string): string {
  const last4 = value.slice(-4);
  for (const { prefix } of PREFIX_PATTERNS) {
    if (value.startsWith(prefix)) {
      const middleLen = Math.max(value.length - prefix.length - 4, 4);
      return `${prefix}${'•'.repeat(middleLen)}${last4}`;
    }
  }
  const dotsLen = Math.max(value.length - 4, 8);
  return `${'•'.repeat(dotsLen)}${last4}`;
}

/** Infer the SecretGroup from a key name. */
export function inferGroup(keyName: string): SecretGroup {
  const upper = keyName.toUpperCase();
  if (upper.includes('GITHUB')) return 'github';
  if (upper.includes('ANTHROPIC')) return 'anthropic';
  if (upper.includes('OPENROUTER')) return 'openrouter';
  return 'custom';
}

/** Validate that a key name is UPPER_SNAKE_CASE. */
export function isValidKeyName(name: string): boolean {
  return /^[A-Z][A-Z0-9_]*$/.test(name);
}

/**
 * Validate a secret value against its service format regex.
 * Returns null if valid, or an error hint string if invalid.
 */
export function validateSecretValue(
  value: string,
  group: SecretGroup,
): string | null {
  const regex = SECRET_FORMAT_REGEXES[group] ?? SECRET_FORMAT_REGEXES.custom;
  if (regex.test(value)) return null;
  return SECRET_FORMAT_HINTS[group] ?? SECRET_FORMAT_HINTS.custom;
}
