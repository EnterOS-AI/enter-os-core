'use client';

import { useState, useEffect, useCallback } from 'react';
import { api } from '@/lib/api';
import { Spinner } from '@/components/Spinner';
import { ConfirmDialog } from '@/components/ConfirmDialog';

interface Token {
  id: string;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
}

interface TokensTabProps {
  workspaceId: string;
}

export function TokensTab({ workspaceId }: TokensTabProps) {
  const [tokens, setTokens] = useState<Token[]>([]);
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [revokeTarget, setRevokeTarget] = useState<Token | null>(null);
  const [error, setError] = useState<string | null>(null);

  const fetchTokens = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.get<{ tokens: Token[]; count: number }>(
        `/workspaces/${workspaceId}/tokens`
      );
      setTokens(data.tokens);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load tokens');
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    fetchTokens();
  }, [fetchTokens]);

  const handleCreate = async () => {
    setCreating(true);
    setError(null);
    try {
      const data = await api.post<{ auth_token: string }>(`/workspaces/${workspaceId}/tokens`);
      setNewToken(data.auth_token);
      fetchTokens();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to create token');
    } finally {
      setCreating(false);
    }
  };

  const handleRevoke = async (token: Token) => {
    setError(null);
    try {
      await api.del(`/workspaces/${workspaceId}/tokens/${token.id}`);
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
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold text-ink">API Tokens</h3>
          <p className="text-[10px] text-ink-soft mt-0.5">
            Bearer tokens for authenticating API calls to this workspace.
          </p>
        </div>
        <button
          onClick={handleCreate}
          disabled={creating}
          className="px-3 py-1.5 bg-accent-strong/20 hover:bg-accent-strong/30 border border-accent/30 rounded-lg text-[11px] text-accent font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed flex items-center gap-1.5"
        >
          {creating ? <><Spinner size="sm" /> Creating...</> : '+ New Token'}
        </button>
      </div>

      {/* Newly created token — show once */}
      {newToken && (
        <div className="bg-emerald-950/30 border border-emerald-800/40 rounded-lg p-3 space-y-2">
          <div className="flex items-center gap-2">
            <span className="text-[10px] text-good font-semibold uppercase tracking-wider">New Token Created</span>
            <span className="text-[9px] text-good/70">Copy now — it won't be shown again</span>
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
        <div className="flex items-center justify-center gap-2 py-6 text-ink-soft text-xs">
          <Spinner /> Loading tokens...
        </div>
      ) : tokens.length === 0 ? (
        <div className="text-center py-6">
          <p className="text-xs text-ink-soft">No active tokens</p>
          <p className="text-[10px] text-ink-soft mt-1">
            Create a token to authenticate API calls.
          </p>
        </div>
      ) : (
        <div className="space-y-1.5">
          {tokens.map((t) => (
            <div
              key={t.id}
              className="flex items-center justify-between bg-surface-card/40 border border-line/30 rounded-lg px-3 py-2"
            >
              <div className="flex items-center gap-3 min-w-0">
                <code className="text-[11px] font-mono text-ink-mid bg-surface-sunken/60 px-1.5 py-0.5 rounded">
                  {t.prefix}...
                </code>
                <div className="text-[9px] text-ink-soft space-x-3">
                  <span>Created {formatAge(t.created_at)}</span>
                  {t.last_used_at && (
                    <span>Last used {formatAge(t.last_used_at)}</span>
                  )}
                </div>
              </div>
              <button
                onClick={() => setRevokeTarget(t)}
                className="text-[10px] text-bad/70 hover:text-bad transition-colors px-2 py-1"
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
        title="Revoke Token"
        message={`Revoke token ${revokeTarget?.prefix}...? Any agent or script using this token will immediately lose access.`}
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
