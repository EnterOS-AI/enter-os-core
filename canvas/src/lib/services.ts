import type { ServiceConfig, SecretGroup } from '@/types/secrets';

/**
 * Static service registry — used for LIST-view rendering: the
 * per-group icon, the "get a key" docs link shown as a hint once
 * the user types a matching key name, and the test-connection
 * routing for the 3 providers with backend test endpoints.
 *
 * Keys not matching any known service fall into the "custom" catch-all.
 *
 * Note (2026-04-22): the Add-Key form no longer uses this as a
 * user-facing dropdown. It reads keyNames[0] via getDefaultKeyName
 * — still referenced by a couple of legacy call sites — and the
 * Add form's autocomplete source lives in KEY_NAME_SUGGESTIONS
 * below. SERVICES is purely for post-save display + test routing.
 */
export const SERVICES: Record<SecretGroup, ServiceConfig> = {
  github: {
    label: 'GitHub',
    icon: 'github',
    keyNames: ['GITHUB_TOKEN'],
    docsUrl: 'https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens',
    testSupported: true,
  },
  anthropic: {
    label: 'Anthropic',
    icon: 'anthropic',
    keyNames: ['ANTHROPIC_API_KEY'],
    docsUrl: 'https://docs.anthropic.com/en/api/getting-started',
    testSupported: true,
  },
  openrouter: {
    label: 'OpenRouter',
    icon: 'openrouter',
    keyNames: ['OPENROUTER_API_KEY'],
    docsUrl: 'https://openrouter.ai/docs/api-keys',
    testSupported: true,
  },
  custom: {
    label: 'Other',
    icon: 'key',
    keyNames: [],
    docsUrl: '',
    testSupported: false,
  },
};

/** Ordered list of groups for consistent rendering. */
export const SERVICE_GROUP_ORDER: SecretGroup[] = [
  'github',
  'anthropic',
  'openrouter',
  'custom',
];

/** Get default key name when a service is selected in the Add form. */
export function getDefaultKeyName(group: SecretGroup): string {
  return SERVICES[group].keyNames[0] ?? '';
}

/**
 * Autocomplete suggestions for the Add-Key form's key-name input.
 *
 * Covers the providers hermes-agent supports natively + the common
 * infra keys (GitHub, platform-side). Adding a new provider here is
 * a one-line change — the Add form picks it up via <datalist>, and
 * classification (for validation + list grouping) comes from
 * inferGroup in lib/validation/secret-formats.ts.
 *
 * Order: alphabetical for stable display in autocomplete popups.
 */
export const KEY_NAME_SUGGESTIONS: readonly string[] = [
  'AI_GATEWAY_API_KEY',
  'ANTHROPIC_API_KEY',
  'ARCEEAI_API_KEY',
  'COPILOT_GITHUB_TOKEN',
  'DASHSCOPE_API_KEY',
  'DEEPSEEK_API_KEY',
  'GEMINI_API_KEY',
  'GH_TOKEN',
  'GITHUB_TOKEN',
  'GLM_API_KEY',
  'GOOGLE_API_KEY',
  'HERMES_API_KEY',
  'HF_TOKEN',
  'KILOCODE_API_KEY',
  'KIMI_API_KEY',
  'KIMI_CN_API_KEY',
  'MINIMAX_API_KEY',
  'MINIMAX_CN_API_KEY',
  'NOUS_API_KEY',
  'NVIDIA_API_KEY',
  'OLLAMA_API_KEY',
  'OPENAI_API_KEY',
  'OPENCODE_GO_API_KEY',
  'OPENCODE_ZEN_API_KEY',
  'OPENROUTER_API_KEY',
  'XIAOMI_API_KEY',
] as const;
