'use client';

import { useState, useEffect, useCallback } from 'react';
import { api } from '@/lib/api';
import { Spinner } from '@/components/Spinner';
import { ConfirmDialog } from '@/components/ConfirmDialog';

/**
 * Organization-scoped API keys.
 *
 * Full-admin bearer tokens for the tenant platform. Unlike TokensTab
 * (which mints workspace-scoped tokens for agents), these authenticate
 * ANY admin endpoint on the tenant — all workspaces, all settings,
 * all bundles + templates. Designed for:
 *
 *   - External integrations (Zapier, n8n, custom scripts)
 *   - AI agents that need full-org visibility
 *   - CLI tools built against the tenant API
 *
 * Security model for beta: one token tier, full admin access. Later
 * work adds scopes (READ / WORKSPACE-WRITE / ORG-ADMIN). See the
 * `future-work` section in docs/architecture/org-api-keys.md.
 */

interface OrgToken {
  id: string;
  prefix: string;
  name?: string;
  created_by?: string;
  created_at: string;
  last_used_at?: string;
}

export function OrgTokensTab() {
  const [tokens, setTokens] = useState<OrgToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [newTokenName, setNewTokenName] = useState('');
  const [copied, setCopied] = useState(false);
  const [revokeTarget, setRevokeTarget] = useState<OrgToken | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Pending name-input for the create flow. Separate from newTokenName
  // (which freezes the label used at the moment of creation) so
  // switching focus between inputs doesn't wipe what was typed.
  const [nameInput, setNameInput] = useState('');

  const fetchTokens = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.get<{ tokens: OrgToken[]; count: number }>(
        `/org/tokens`,
      );
      setTokens(data.tokens);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load tokens');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchTokens();
  }, [fetchTokens]);

  const handleCreate = async () => {
    setCreating(true);
    setError(null);
    try {
      const data = await api.post<{ auth_token: string; prefix: string }>(
        `/org/tokens`,
        nameInput.trim() ? { name: nameInput.trim() } : {},
      );
      setNewToken(data.auth_token);
      setNewTokenName(nameInput.trim());
      setNameInput('');
      fetchTokens();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to create token');
    } finally {
      setCreating(false);
    }
  };

  const handleRevoke = async (token: OrgToken) => {
    setError(null);
    try {
      await api.del(`/org/tokens/${token.id}`);
      setRevokeTarget(null);
      fetchTokens();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to revoke token');
    }
  };

  const handleCopy = () => {
    if (newToken) {
      navigator.clipboard.writeText(newToken);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  return (
    <div className="p-4 space-y-4">
      <div>
        <div className="flex items-center justify-between mb-1">
          <h3 className="text-sm font-semibold text-ink">
            Organization API Keys
          </h3>
        </div>
        <p className="text-[10px] text-ink-soft leading-relaxed">
          Full-admin bearer tokens for this organization. Use with external
          integrations, CLI tools, or AI agents that need to manage
          workspaces, settings, and secrets. Each key has the same
          privileges as logging in — treat like a password.
        </p>
      </div>

      {/* Create form */}
      <div className="flex gap-2 items-stretch">
        <input
          type="text"
          value={nameInput}
          onChange={(e) => setNameInput(e.target.value)}
          placeholder="Label (e.g. zapier, my-ci)"
          maxLength={100}
          aria-label="Organization API key label"
          className="flex-1 text-[11px] bg-surface-sunken/60 border border-line/50 rounded px-2 py-1.5 text-ink placeholder-zinc-600"
        />
        <button
          onClick={handleCreate}
          disabled={creating}
          className="px-3 py-1.5 bg-accent-strong/20 hover:bg-accent-strong/30 border border-accent/30 rounded-lg text-[11px] text-accent font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed flex items-center gap-1.5"
        >
          {creating ? (
            <>
              <Spinner size="sm" /> Creating...
            </>
          ) : (
            '+ New Key'
          )}
        </button>
      </div>

      {/* Newly created token — show once */}
      {newToken && (
        <div className="bg-emerald-950/30 border border-emerald-800/40 rounded-lg p-3 space-y-2">
          <div className="flex items-center gap-2">
            <span className="text-[10px] text-good font-semibold uppercase tracking-wider">
              {newTokenName ? `New Key: ${newTokenName}` : 'New Key Created'}
            </span>
            <span className="text-[9px] text-good/70">
              Copy now — it won't be shown again
            </span>
          </div>
          <div className="flex items-center gap-2">
            <code className="flex-1 text-[11px] text-emerald-200 bg-emerald-950/50 px-2 py-1.5 rounded font-mono break-all select-all">
              {newToken}
            </code>
            <button
              onClick={handleCopy}
              className="shrink-0 px-2 py-1.5 bg-emerald-800/40 hover:bg-emerald-700/50 border border-emerald-700/40 rounded text-[10px] text-good transition-colors"
            >
              {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
          <button
            onClick={() => setNewToken(null)}
            className="text-[9px] text-good/60 hover:text-good transition-colors"
          >
            Dismiss
          </button>
        </div>
      )}

      {error && (
        <div className="px-3 py-2 bg-red-950/40 border border-red-800/50 rounded-lg text-[10px] text-bad">
          {error}
        </div>
      )}

      {/* Token list */}
      {loading ? (
        <div role="status" aria-live="polite" className="flex items-center justify-center gap-2 py-6 text-ink-soft text-xs">
          <Spinner /> Loading keys...
        </div>
      ) : tokens.length === 0 ? (
        <div className="text-center py-6">
          <p className="text-xs text-ink-soft">No active keys</p>
          <p className="text-[10px] text-ink-soft mt-1">
            Create a key above to authenticate API calls to this organization.
          </p>
        </div>
      ) : (
        <div className="space-y-1.5">
          {tokens.map((t) => (
            <div
              key={t.id}
              className="flex items-center justify-between bg-surface-card/40 border border-line/30 rounded-lg px-3 py-2"
            >
              <div className="flex items-center gap-3 min-w-0 flex-1">
                <code className="text-[11px] font-mono text-ink-mid bg-surface-sunken/60 px-1.5 py-0.5 rounded shrink-0">
                  {t.prefix}...
                </code>
                <div className="flex flex-col min-w-0">
                  {t.name && (
                    <span className="text-[11px] text-ink truncate">
                      {t.name}
                    </span>
                  )}
                  <div className="text-[9px] text-ink-soft space-x-3">
                    <span>Created {formatAge(t.created_at)}</span>
                    {t.last_used_at && (
                      <span>Last used {formatAge(t.last_used_at)}</span>
                    )}
                  </div>
                </div>
              </div>
              <button
                onClick={() => setRevokeTarget(t)}
                className="text-[10px] text-bad/70 hover:text-bad transition-colors px-2 py-1 shrink-0"
              >
                Revoke
              </button>
            </div>
          ))}
        </div>
      )}

      {/* Revoke confirmation */}
      <ConfirmDialog
        open={!!revokeTarget}
        title="Revoke API Key"
        message={`Revoke ${revokeTarget?.name ? `"${revokeTarget.name}" ` : ''}(${revokeTarget?.prefix}...)? Any integration using this key will immediately lose access.`}
        confirmLabel="Revoke"
        confirmVariant="danger"
        onConfirm={() => revokeTarget && handleRevoke(revokeTarget)}
        onCancel={() => setRevokeTarget(null)}
      />
    </div>
  );
}

function formatAge(timestamp: string): string {
  const diff = Date.now() - new Date(timestamp).getTime();
  if (diff < 60000) return 'just now';
  if (diff < 3600000) return `${Math.floor(diff / 60000)}m ago`;
  if (diff < 86400000) return `${Math.floor(diff / 3600000)}h ago`;
  return `${Math.floor(diff / 86400000)}d ago`;
}
