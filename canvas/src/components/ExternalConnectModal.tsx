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
}

interface Props {
  info: ExternalConnectionInfo | null;
  onClose: () => void;
}

type Tab = "python" | "curl" | "fields";

export function ExternalConnectModal({ info, onClose }: Props) {
  const [tab, setTab] = useState<Tab>("python");
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

  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/60 z-50" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(720px,92vw)] -translate-x-1/2 -translate-y-1/2 rounded-xl bg-zinc-900 border border-zinc-700 p-6 shadow-2xl">
          <Dialog.Title className="text-lg font-semibold text-white">
            Connect your external agent
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-zinc-400">
            Paste the snippet below into your agent&apos;s deployment. The
            auth token is shown <span className="text-amber-400">only once</span>
            {" "}— save it somewhere safe before closing this dialog.
          </Dialog.Description>

          {/* Tabs */}
          <div
            role="tablist"
            aria-label="Connection snippet format"
            className="mt-4 flex gap-1 border-b border-zinc-800"
          >
            {(["python", "curl", "fields"] as Tab[]).map((t) => (
              <button
                key={t}
                type="button"
                role="tab"
                aria-selected={tab === t}
                onClick={() => setTab(t)}
                className={`px-3 py-2 text-sm border-b-2 -mb-px transition-colors ${
                  tab === t
                    ? "border-blue-500 text-white"
                    : "border-transparent text-zinc-500 hover:text-zinc-300"
                }`}
              >
                {t === "python" ? "Python SDK" : t === "curl" ? "curl" : "Fields"}
              </button>
            ))}
          </div>

          {/* Snippet area */}
          <div className="mt-3">
            {tab === "python" && (
              <SnippetBlock
                value={filledPython}
                label="Python (recommended — includes heartbeat loop)"
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
              className="px-4 py-2 text-sm rounded-lg bg-zinc-800 hover:bg-zinc-700 text-zinc-200"
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
        <span className="text-xs text-zinc-500">{label}</span>
        <button
          type="button"
          onClick={onCopy}
          className="text-xs px-2 py-1 rounded bg-blue-600/80 hover:bg-blue-500 text-white"
        >
          {copied ? "Copied!" : "Copy"}
        </button>
      </div>
      <pre className="text-xs bg-zinc-950 border border-zinc-800 rounded-lg p-3 max-h-80 overflow-auto whitespace-pre-wrap break-all font-mono text-zinc-200">
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
      <span className="text-xs text-zinc-500 w-36 shrink-0">{label}</span>
      <code
        className={`flex-1 text-xs bg-zinc-950 border border-zinc-800 rounded px-2 py-1 text-zinc-200 break-all ${mono ? "font-mono" : ""}`}
      >
        {value || "(missing)"}
      </code>
      <button
        type="button"
        onClick={onCopy}
        disabled={!value}
        className="text-xs px-2 py-1 rounded bg-zinc-800 hover:bg-zinc-700 text-zinc-200 disabled:opacity-40"
      >
        {copied ? "Copied!" : "Copy"}
      </button>
    </div>
  );
}
