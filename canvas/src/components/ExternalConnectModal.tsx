'use client';

// ExternalConnectModal — shown once after creating a runtime="external"
// workspace. Surfaces the workspace_auth_token + ready-to-paste snippets
// so the operator can hand them to whoever runs their off-host agent
// without piecing together the register payload from docs.
//
// Security posture:
//   - The auth_token is visible once. After the modal closes, the value
//     is unrecoverable (the /workspaces/:id read endpoints never echo it).
//     UI warns the operator before they dismiss.
//   - A "copy to clipboard" button uses the navigator.clipboard API which
//     is same-origin and requires user gesture — no cross-origin leak.
//   - Snippets use placeholders for the operator's own public URL
//     ($AGENT_URL). They ARE NOT filled in server-side because the
//     server doesn't know where the operator's agent will live.

import { useCallback, useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";

export interface ExternalConnectionInfo {
  workspace_id: string;
  platform_url: string;
  auth_token: string;
  registry_endpoint: string;
  heartbeat_endpoint: string;
  curl_register_template: string;
  python_snippet: string;
  // Claude Code channel plugin snippet — for operators whose external
  // agent IS a Claude Code session. Polling-based; no tunnel required.
  // Optional in the type for backward compat with platforms that
  // haven't shipped molecule-core PR #2304 yet (older response payload
  // omits the field; tab is hidden if empty).
  claude_code_channel_snippet?: string;
  // Universal MCP snippet — runtime-agnostic outbound tool path via
  // the `molecule-mcp` console script in the
  // molecule-ai-workspace-runtime PyPI wheel. Works with any MCP-aware
  // agent runtime (Claude Code, hermes, codex, third-party). Outbound-
  // only: pair with claude_code_channel or python tabs for heartbeat
  // + inbound. Optional for backward compat with platforms that
  // haven't shipped PR #2413 yet.
  universal_mcp_snippet?: string;
}

interface Props {
  info: ExternalConnectionInfo | null;
  onClose: () => void;
}

type Tab = "python" | "curl" | "claude" | "mcp" | "fields";

export function ExternalConnectModal({ info, onClose }: Props) {
  // Default to Claude Code when the platform offers it — that's the
  // newest + simplest path (no tunnel needed). Falls back to Python
  // for older platform builds that don't ship the snippet.
  const initialTab: Tab = info?.claude_code_channel_snippet ? "claude" : "python";
  const [tab, setTab] = useState<Tab>(initialTab);
  const [copiedKey, setCopiedKey] = useState<string | null>(null);

  const copy = useCallback(async (value: string, key: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setCopiedKey(key);
      // Auto-clear the "Copied!" label after 1.5s so a second copy
      // attempt feels responsive — without the reset, the second
      // click appears as a no-op.
      window.setTimeout(() => setCopiedKey(null), 1500);
    } catch {
      // Fallback for browsers that refuse clipboard access (http://
      // over insecure origin, Safari private mode, etc.). We surface
      // a minimal textarea so the operator can manually copy.
      const el = document.getElementById(`fallback-${key}`) as HTMLTextAreaElement | null;
      if (el) {
        el.select();
      }
    }
  }, []);

  if (!info) return null;

  // Python snippet is stamped server-side with workspace_id +
  // platform_url but leaves AUTH_TOKEN as a "<paste …>" placeholder
  // (that's what we're showing in the modal). Fill in the real
  // token here so the snippet the operator copies is truly ready-to-run.
  const filledPython = info.python_snippet.replace(
    'AUTH_TOKEN    = "<paste from create response>"',
    `AUTH_TOKEN    = "${info.auth_token}"`,
  );
  const filledCurl = info.curl_register_template.replace(
    'WORKSPACE_AUTH_TOKEN="<paste from create response>"',
    `WORKSPACE_AUTH_TOKEN="${info.auth_token}"`,
  );
  // The channel snippet asks the operator to paste the auth_token into
  // the .env file's MOLECULE_WORKSPACE_TOKENS field. Stamp it server-side
  // here so the copy-paste-block is truly ready-to-run.
  const filledChannel = info.claude_code_channel_snippet?.replace(
    'MOLECULE_WORKSPACE_TOKENS=<paste auth_token from create response>',
    `MOLECULE_WORKSPACE_TOKENS=${info.auth_token}`,
  );
  // Universal MCP snippet uses MOLECULE_WORKSPACE_TOKEN as the env-var
  // name passed through to molecule-mcp via `claude mcp add ... -- env
  // MOLECULE_WORKSPACE_TOKEN=...`. The placeholder must match the
  // template's literal — pre-2026-04-30 polish this looked for
  // WORKSPACE_AUTH_TOKEN (carryover from the curl tab), which silently
  // skipped the substitution and left "<paste from create response>"
  // visible in the operator's clipboard.
  const filledUniversalMcp = info.universal_mcp_snippet?.replace(
    'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
    `MOLECULE_WORKSPACE_TOKEN="${info.auth_token}"`,
  );

  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/60 z-50" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(720px,92vw)] -translate-x-1/2 -translate-y-1/2 rounded-xl bg-surface-sunken border border-line p-6 shadow-2xl">
          <Dialog.Title className="text-lg font-semibold text-ink">
            Connect your external agent
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-ink-mid">
            Paste the snippet below into your agent&apos;s deployment. The
            auth token is shown <span className="text-warm">only once</span>
            {" "}— save it somewhere safe before closing this dialog.
          </Dialog.Description>

          {/* Tabs */}
          <div
            role="tablist"
            aria-label="Connection snippet format"
            className="mt-4 flex gap-1 border-b border-line"
          >
            {(() => {
              // Build the tab order dynamically. Claude Code first
              // (when offered) since it's the simplest setup; Python
              // SDK second (full register+heartbeat+inbound); Universal
              // MCP third (any MCP-aware runtime, outbound-only); curl
              // for one-shot register; Fields for raw values.
              const tabs: Tab[] = [];
              if (filledChannel) tabs.push("claude");
              tabs.push("python");
              if (filledUniversalMcp) tabs.push("mcp");
              tabs.push("curl", "fields");
              return tabs;
            })().map((t) => (
              <button
                key={t}
                type="button"
                role="tab"
                aria-selected={tab === t}
                onClick={() => setTab(t)}
                className={`px-3 py-2 text-sm border-b-2 -mb-px transition-colors ${
                  tab === t
                    ? "border-accent text-ink"
                    : "border-transparent text-ink-soft hover:text-ink-mid"
                }`}
              >
                {t === "claude"
                  ? "Claude Code"
                  : t === "python"
                  ? "Python SDK"
                  : t === "mcp"
                  ? "Universal MCP"
                  : t === "curl"
                  ? "curl"
                  : "Fields"}
              </button>
            ))}
          </div>

          {/* Snippet area */}
          <div className="mt-3">
            {tab === "claude" && filledChannel && (
              <SnippetBlock
                value={filledChannel}
                label="Claude Code channel — polls workspace's A2A; no tunnel needed"
                copyKey="claude"
                copied={copiedKey === "claude"}
                onCopy={() => copy(filledChannel, "claude")}
              />
            )}
            {tab === "python" && (
              <SnippetBlock
                value={filledPython}
                label="Python SDK — includes heartbeat loop (push-mode, needs public URL)"
                copyKey="python"
                copied={copiedKey === "python"}
                onCopy={() => copy(filledPython, "python")}
              />
            )}
            {tab === "curl" && (
              <SnippetBlock
                value={filledCurl}
                label="curl — one-shot register only (no heartbeat)"
                copyKey="curl"
                copied={copiedKey === "curl"}
                onCopy={() => copy(filledCurl, "curl")}
              />
            )}
            {tab === "mcp" && filledUniversalMcp && (
              <SnippetBlock
                value={filledUniversalMcp}
                label="Universal MCP — standalone register + heartbeat + tools for any MCP-aware runtime (Claude Code, hermes, codex). Pair with Python or Claude Code tab if you need inbound A2A delivery."
                copyKey="mcp"
                copied={copiedKey === "mcp"}
                onCopy={() => copy(filledUniversalMcp, "mcp")}
              />
            )}
            {tab === "fields" && (
              <div className="space-y-2">
                <Field label="workspace_id" value={info.workspace_id} onCopy={() => copy(info.workspace_id, "wsid")} copied={copiedKey === "wsid"} />
                <Field label="platform_url" value={info.platform_url} onCopy={() => copy(info.platform_url, "url")} copied={copiedKey === "url"} />
                <Field
                  label="auth_token"
                  value={info.auth_token}
                  onCopy={() => copy(info.auth_token, "tok")}
                  copied={copiedKey === "tok"}
                  mono
                />
                <Field label="registry_endpoint" value={info.registry_endpoint} onCopy={() => copy(info.registry_endpoint, "reg")} copied={copiedKey === "reg"} />
                <Field label="heartbeat_endpoint" value={info.heartbeat_endpoint} onCopy={() => copy(info.heartbeat_endpoint, "hb")} copied={copiedKey === "hb"} />
              </div>
            )}
          </div>

          <div className="mt-5 flex justify-end gap-2">
            <button
              type="button"
              onClick={onClose}
              className="px-4 py-2 text-sm rounded-lg bg-surface-card hover:bg-surface-card text-ink"
            >
              I&apos;ve saved it — close
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function SnippetBlock({
  value,
  label,
  copied,
  onCopy,
}: {
  value: string;
  label: string;
  copyKey: string;
  copied: boolean;
  onCopy: () => void;
}) {
  return (
    <div>
      <div className="flex items-center justify-between pb-1">
        <span className="text-xs text-ink-soft">{label}</span>
        <button
          type="button"
          onClick={onCopy}
          className="text-xs px-2 py-1 rounded bg-accent-strong/80 hover:bg-accent text-white"
        >
          {copied ? "Copied!" : "Copy"}
        </button>
      </div>
      <pre className="text-xs bg-surface border border-line rounded-lg p-3 max-h-80 overflow-auto whitespace-pre-wrap break-all font-mono text-ink">
        {value}
      </pre>
    </div>
  );
}

function Field({
  label,
  value,
  onCopy,
  copied,
  mono,
}: {
  label: string;
  value: string;
  onCopy: () => void;
  copied: boolean;
  mono?: boolean;
}) {
  return (
    <div className="flex items-center gap-2">
      <span className="text-xs text-ink-soft w-36 shrink-0">{label}</span>
      <code
        className={`flex-1 text-xs bg-surface border border-line rounded px-2 py-1 text-ink break-all ${mono ? "font-mono" : ""}`}
      >
        {value || "(missing)"}
      </code>
      <button
        type="button"
        onClick={onCopy}
        disabled={!value}
        className="text-xs px-2 py-1 rounded bg-surface-card hover:bg-surface-card text-ink disabled:opacity-40"
      >
        {copied ? "Copied!" : "Copy"}
      </button>
    </div>
  );
}
