'use client';

// ExternalConnectionSection — credential lifecycle controls for runtime=external
// workspaces. Surfaced inside ConfigTab when the workspace's runtime is
// "external"; ignored for hermes/claude-code/etc. (those have their own
// restart-mints-token path).
//
// Two affordances:
//
//   1. "Show connection info" (read-only)
//        Fetches GET /workspaces/:id/external/connection. Returns the
//        connect block (PLATFORM_URL, WORKSPACE_ID, all 7 snippets) WITH
//        auth_token="". The modal masks the token field and labels it
//        "rotate to reveal a new token — current token is unrecoverable".
//
//   2. "Rotate credentials" (destructive)
//        POST /workspaces/:id/external/rotate. Revokes any prior live
//        tokens, mints a fresh one, returns the same connect block with
//        auth_token populated. Old credentials stop working IMMEDIATELY —
//        the previously-paired agent will fail auth on its next heartbeat.
//        Confirm dialog explains this before firing.
//
// Reuses the existing ExternalConnectModal so the snippet UX is the
// same as on Create — operators don't have to learn a second modal.

import { useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";

import { api } from "@/lib/api";
import {
  ExternalConnectModal,
  type ExternalConnectionInfo,
} from "../ExternalConnectModal";

interface Props {
  workspaceId: string;
}

export function ExternalConnectionSection({ workspaceId }: Props) {
  const [info, setInfo] = useState<ExternalConnectionInfo | null>(null);
  const [busy, setBusy] = useState<"show" | "rotate" | null>(null);
  const [confirmRotate, setConfirmRotate] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function showConnection() {
    setError(null);
    setBusy("show");
    try {
      const resp = await api.get<{ connection: ExternalConnectionInfo }>(
        `/workspaces/${workspaceId}/external/connection`,
      );
      setInfo(resp.connection);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  async function doRotate() {
    setError(null);
    setBusy("rotate");
    setConfirmRotate(false);
    try {
      const resp = await api.post<{ connection: ExternalConnectionInfo }>(
        `/workspaces/${workspaceId}/external/rotate`,
        {},
      );
      setInfo(resp.connection);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="mx-3 mt-3 p-3 bg-surface-sunken/50 border border-line rounded">
      <h3 className="text-xs text-ink-mid font-medium mb-1">External Connection</h3>
      <p className="text-[10px] text-ink-soft mb-2">
        This workspace runs an external agent. Use these controls to
        re-show the setup snippets or rotate the workspace token.
      </p>

      <div className="flex gap-2 flex-wrap">
        <button
          type="button"
          onClick={showConnection}
          disabled={busy !== null}
          className="px-3 py-1.5 bg-surface-card hover:bg-surface-card text-xs rounded text-ink-mid disabled:opacity-30 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
        >
          {busy === "show" ? "Loading…" : "Show connection info"}
        </button>
        <button
          type="button"
          onClick={() => setConfirmRotate(true)}
          disabled={busy !== null}
          className="px-3 py-1.5 bg-red-900/30 hover:bg-red-900/50 border border-red-800/60 text-xs rounded text-bad disabled:opacity-30 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-red-600/60"
        >
          {busy === "rotate" ? "Rotating…" : "Rotate credentials"}
        </button>
      </div>

      {error && (
        <div className="mt-2 px-2 py-1 bg-red-900/30 border border-red-800 rounded text-[10px] text-bad">
          {error}
        </div>
      )}

      <Dialog.Root open={confirmRotate} onOpenChange={setConfirmRotate}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/60 z-50" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(440px,92vw)] -translate-x-1/2 -translate-y-1/2 rounded-xl bg-surface-sunken border border-line p-5 shadow-2xl">
            <Dialog.Title className="text-sm font-medium text-ink mb-2">
              Rotate workspace credentials?
            </Dialog.Title>
            <Dialog.Description className="text-xs text-ink-mid mb-4 leading-relaxed">
              This will mint a new <code className="font-mono">workspace_auth_token</code> and{' '}
              <strong>immediately invalidate the current one</strong>. Your external
              agent will start failing authentication on its next heartbeat
              until you redeploy it with the new token.
            </Dialog.Description>
            <div className="flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setConfirmRotate(false)}
                className="px-3 py-1.5 bg-surface-card text-xs rounded text-ink-mid"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={doRotate}
                className="px-3 py-1.5 bg-red-700 hover:bg-red-600 text-xs rounded text-white"
              >
                Rotate
              </button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      <ExternalConnectModal info={info} onClose={() => setInfo(null)} />
    </div>
  );
}
