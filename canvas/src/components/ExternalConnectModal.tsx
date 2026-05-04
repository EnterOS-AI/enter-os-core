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

type Tab = "python" | "curl" | "claude" | "mcp" | "hermes" | "codex" | "openclaw" | "fields";

// Per-tab help metadata: docs link, where-to-install link, common errors.
// All URLs verified against repo content (docs/guides/* file paths map to
// docs.molecule.ai/docs/guides/*; canonical hostname confirmed by existing
// blog post canonical metadata) or against the snippet text the operator
// just copied. Never linking to a URL that wasn't already in product —
// dead links here defeat the purpose of "more comprehensive instructions."
const TAB_HELP: Record<
  Tab,
  {
    docsUrl?: string;
    docsLabel?: string;
    downloadUrl?: string;
    downloadLabel?: string;
    commonIssues?: { symptom: string; check: string }[];
  }
> = {
  mcp: {
    docsUrl: "https://docs.molecule.ai/docs/guides/mcp-server-setup",
    docsLabel: "MCP server setup guide",
    downloadUrl: "https://pypi.org/project/molecule-ai-workspace-runtime/",
    downloadLabel: "molecule-ai-workspace-runtime on PyPI",
    commonIssues: [
      {
        symptom: "Tools not appearing in your agent",
        check:
          "Run `claude mcp list` (or your runtime's equivalent) — the molecule entry should be listed. If missing, re-run the `claude mcp add` line.",
      },
      {
        symptom: "ConnectionRefused / DNS error on first call",
        check:
          "PLATFORM_URL must include the scheme (https://) and have no trailing slash. Verify with `curl $PLATFORM_URL/healthz`.",
      },
    ],
  },
  python: {
    docsUrl:
      "https://docs.molecule.ai/docs/guides/external-agent-registration",
    docsLabel: "External agent registration guide",
    downloadUrl: "https://pypi.org/project/molecule-ai-workspace-runtime/",
    downloadLabel: "molecule-ai-workspace-runtime on PyPI",
    commonIssues: [
      {
        symptom: "401 from /heartbeat",
        check:
          "AUTH_TOKEN expired or wrong workspace_id. Tokens are shown only once at create time — re-create the workspace to get a fresh token.",
      },
      {
        symptom: "AGENT_URL not reachable from platform",
        check:
          "Public HTTPS URL required for inbound A2A. Use ngrok or Cloudflare Tunnel if your agent is behind NAT.",
      },
    ],
  },
  claude: {
    docsUrl:
      "https://docs.molecule.ai/docs/guides/external-agent-registration",
    docsLabel: "External agent registration guide",
    downloadUrl: "https://claude.com/claude-code",
    downloadLabel: "Claude Code (claude.com)",
    commonIssues: [
      {
        symptom: "plugin not installed",
        check:
          "Run `/plugin marketplace add Molecule-AI/molecule-mcp-claude-channel` then `/plugin install molecule@molecule-mcp-claude-channel` inside Claude Code, then `/reload-plugins`.",
      },
      {
        symptom: "not on the approved channels allowlist",
        check:
          "Custom channels need `--dangerously-load-development-channels` on the launch command. Team/Enterprise orgs need admin to set `channelsEnabled` + `allowedChannelPlugins` in claude.ai admin settings.",
      },
      {
        symptom: "Inbound messages not arriving",
        check:
          "Check stderr for `molecule channel: connected — watching N workspace(s)`. Verify ~/.claude/channels/molecule/.env has the right PLATFORM_URL + token.",
      },
    ],
  },
  hermes: {
    docsUrl:
      "https://docs.molecule.ai/docs/guides/external-agent-registration",
    docsLabel: "External agent registration guide",
    downloadUrl: "https://github.com/NousResearch/hermes-agent",
    downloadLabel: "hermes-agent (NousResearch)",
    commonIssues: [
      {
        symptom: "Gateway start failure",
        check:
          "Tail ~/.hermes/gateway.log. YAML duplicate-key in config.yaml is the most common cause — `gateway:` block must appear exactly once.",
      },
      {
        symptom: "Plugin not discovered after install",
        check:
          "Run `pip show hermes-channel-molecule` to confirm install. Some hermes builds need `hermes plugin reload` before the new platform_plugins entry takes effect.",
      },
    ],
  },
  codex: {
    docsUrl: "https://docs.molecule.ai/docs/guides/mcp-server-setup",
    docsLabel: "MCP server setup guide",
    downloadUrl: "https://github.com/openai/codex",
    downloadLabel: "openai/codex",
    commonIssues: [
      {
        symptom: "[mcp_servers.molecule] not loaded",
        check:
          "Codex must be ≥ 0.57. Check with `codex --version`; upgrade via `npm install -g @openai/codex@latest`.",
      },
      {
        symptom: "TOML parse error after re-running setup",
        check:
          "TOML rejects duplicate `[mcp_servers.molecule]` tables. Open ~/.codex/config.toml and remove the old block before pasting the new one.",
      },
    ],
  },
  openclaw: {
    docsUrl: "https://docs.molecule.ai/docs/guides/mcp-server-setup",
    docsLabel: "MCP server setup guide",
    commonIssues: [
      {
        symptom: "Gateway not starting",
        check:
          "Tail ~/.openclaw/gateway.log. The loopback bind requires :18789 to be free — check with `lsof -iTCP:18789`.",
      },
      {
        symptom: "openclaw mcp set rejected",
        check:
          "The heredoc generates JSON; verify it parsed by running `jq < ~/.openclaw/mcp/molecule.json`. Re-run `openclaw mcp set` if the file is malformed.",
      },
    ],
  },
  curl: {
    docsUrl:
      "https://docs.molecule.ai/docs/guides/external-agent-registration",
    docsLabel: "External agent registration guide",
    commonIssues: [
      {
        symptom: "401 / 403 on register",
        check:
          "WORKSPACE_AUTH_TOKEN must be the value shown at workspace create. Tokens are shown only once.",
      },
    ],
  },
  fields: {
    docsUrl:
      "https://docs.molecule.ai/docs/guides/external-agent-registration",
    docsLabel: "External agent registration guide",
  },
};

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
  // Hermes channel snippet — for operators whose external agent IS a
  // hermes-agent session. Routes A2A traffic into the hermes gateway
  // via the molecule-channel plugin (Molecule-AI/hermes-channel-molecule).
  // Long-poll based (no tunnel) — same UX shape as the Claude Code
  // channel tab. Gives hermes true push parity. Optional for backward
  // compat with platforms that haven't shipped this PR yet.
  hermes_channel_snippet?: string;
  // Codex MCP config snippet — wires the molecule MCP server into
  // ~/.codex/config.toml so codex agents can call platform tools.
  // Outbound-tools-only today (codex's MCP client doesn't route
  // notifications/*); push parity would need a separate bridge daemon.
  codex_snippet?: string;
  // OpenClaw MCP config snippet — wires molecule MCP + starts the
  // openclaw gateway on loopback. Outbound-tools-only today; push
  // parity on an external openclaw needs a sessions.steer bridge.
  openclaw_snippet?: string;
}

interface Props {
  info: ExternalConnectionInfo | null;
  onClose: () => void;
}

export function ExternalConnectModal({ info, onClose }: Props) {
  // Default to Universal MCP when the platform offers it — runtime-
  // agnostic outbound tool path that works for any MCP-aware runtime
  // (Claude Code, hermes, codex, etc.) and lets operators inspect the
  // primitives before picking a runtime-specific tab. Python SDK is
  // the fallback for platforms predating the universal_mcp_snippet
  // field. Pre-2026-05-03 the default was "claude" (Claude Code first)
  // but operators using non-Claude runtimes opened to a tab they had
  // to skip past — universal MCP works for everyone as a starting
  // point and the runtime-specific tabs are still one click away.
  const initialTab: Tab = info?.universal_mcp_snippet
    ? "mcp"
    : "python";
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
  // Hermes channel snippet uses MOLECULE_WORKSPACE_TOKEN (same env-var
  // name as Universal MCP). Stamp the auth_token in so the operator's
  // copy-paste is fully ready-to-run.
  const filledHermes = info.hermes_channel_snippet?.replace(
    'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
    `MOLECULE_WORKSPACE_TOKEN="${info.auth_token}"`,
  );
  // Codex + OpenClaw snippets carry the placeholder inside the
  // generated config block (TOML / JSON respectively). Stamp the
  // token in so the copy-paste is one less manual edit.
  const filledCodex = info.codex_snippet?.replace(
    'MOLECULE_WORKSPACE_TOKEN = "<paste from create response>"',
    `MOLECULE_WORKSPACE_TOKEN = "${info.auth_token}"`,
  );
  const filledOpenClaw = info.openclaw_snippet?.replace(
    'WORKSPACE_TOKEN="<paste from create response>"',
    `WORKSPACE_TOKEN="${info.auth_token}"`,
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
              // Tab order: Universal MCP first (default, runtime-
              // agnostic primitives), then runtime-specific channel/
              // SDK tabs, then curl + Fields. Each runtime tab only
              // appears when the platform supplies the snippet — no
              // dead "tab missing snippet" UX.
              const tabs: Tab[] = [];
              if (filledUniversalMcp) tabs.push("mcp");
              tabs.push("python");
              if (filledChannel) tabs.push("claude");
              if (filledHermes) tabs.push("hermes");
              if (filledCodex) tabs.push("codex");
              if (filledOpenClaw) tabs.push("openclaw");
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
                  : t === "hermes"
                  ? "Hermes"
                  : t === "codex"
                  ? "Codex"
                  : t === "openclaw"
                  ? "OpenClaw"
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
            {tab === "hermes" && filledHermes && (
              <SnippetBlock
                value={filledHermes}
                label="Hermes channel — bridges this workspace's A2A traffic into your hermes-agent session as platform messages (push parity with Claude Code). Long-poll based; no tunnel needed."
                copyKey="hermes"
                copied={copiedKey === "hermes"}
                onCopy={() => copy(filledHermes, "hermes")}
              />
            )}
            {tab === "codex" && filledCodex && (
              <SnippetBlock
                value={filledCodex}
                label="Codex MCP config — wires the molecule MCP server into ~/.codex/config.toml. Outbound tools today; inbound A2A push needs the Python SDK tab paired in (codex's MCP runtime doesn't route arbitrary notifications/* yet)."
                copyKey="codex"
                copied={copiedKey === "codex"}
                onCopy={() => copy(filledCodex, "codex")}
              />
            )}
            {tab === "openclaw" && filledOpenClaw && (
              <SnippetBlock
                value={filledOpenClaw}
                label="OpenClaw MCP config — wires the molecule MCP server via openclaw mcp set + starts the gateway on loopback. Outbound tools today; inbound A2A push on an external openclaw needs the Python SDK tab paired in (a sessions.steer bridge daemon is future work)."
                copyKey="openclaw"
                copied={copiedKey === "openclaw"}
                onCopy={() => copy(filledOpenClaw, "openclaw")}
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
            <HelpBlock help={TAB_HELP[tab]} />
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

// HelpBlock — collapsible "Need help?" section under each tab's snippet.
// Renders only the keys present in the per-tab help metadata (no empty
// sections). Closed by default so the snippet stays the visual focus;
// operators with a working setup never see this. Uses native <details>
// for keyboard accessibility (Tab + Enter) without extra ARIA wiring.
function HelpBlock({
  help,
}: {
  help: (typeof TAB_HELP)[Tab] | undefined;
}) {
  if (!help) return null;
  const { docsUrl, docsLabel, downloadUrl, downloadLabel, commonIssues } = help;
  if (!docsUrl && !downloadUrl && !commonIssues?.length) return null;

  return (
    <details className="mt-3 border border-line rounded-lg bg-surface text-xs">
      <summary className="cursor-pointer select-none px-3 py-2 text-ink-mid hover:text-ink">
        Need help? — install link, docs, common errors
      </summary>
      <div className="px-3 pb-3 pt-1 space-y-2">
        {downloadUrl && (
          <div>
            <span className="text-ink-soft">Where to install: </span>
            <a
              href={downloadUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="text-accent underline hover:text-accent-strong"
            >
              {downloadLabel || downloadUrl}
            </a>
          </div>
        )}
        {docsUrl && (
          <div>
            <span className="text-ink-soft">Documentation: </span>
            <a
              href={docsUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="text-accent underline hover:text-accent-strong"
            >
              {docsLabel || docsUrl}
            </a>
          </div>
        )}
        {commonIssues && commonIssues.length > 0 && (
          <div>
            <div className="text-ink-soft mb-1">Common errors:</div>
            <ul className="space-y-1.5 pl-3">
              {commonIssues.map((issue, i) => (
                <li key={i}>
                  <code className="text-warm font-mono">{issue.symptom}</code>
                  <span className="text-ink-mid"> — {issue.check}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </details>
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
