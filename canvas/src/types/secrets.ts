export type SecretStatus = 'verified' | 'invalid' | 'unverified';

export type SecretGroup = 'github' | 'anthropic' | 'openrouter' | 'custom';

export interface Secret {
  name: string;
  masked_value: string;
  group: SecretGroup;
  status: SecretStatus;
  updated_at: string;
}

export interface ServiceConfig {
  label: string;
  icon: string;
  keyNames: string[];
  docsUrl: string;
  testSupported: boolean;
}

export type TestConnectionState = 'idle' | 'testing' | 'success' | 'failure';
