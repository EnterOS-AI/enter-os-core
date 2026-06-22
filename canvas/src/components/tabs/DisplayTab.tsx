"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import type RFB from "@novnc/novnc";

interface DisplayStatus {
  available: boolean;
  reason?: string;
  mode?: string;
  status?: string;
  protocol?: string;
  width?: number;
  height?: number;
}

interface DisplayControlStatus {
  controller: "none" | "user" | "agent";
  controlled_by?: string;
  expires_at?: string;
  session_url?: string;
}

interface Props {
  workspaceId: string;
}

export function DisplayTab({ workspaceId }: Props) {
  const [status, setStatus] = useState<DisplayStatus | null>(null);
  const [control, setControl] = useState<DisplayControlStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [controlError, setControlError] = useState<string | null>(null);
  const [controlBusy, setControlBusy] = useState(false);
  const [sessionUrl, setSessionUrl] = useState<string | null>(null);
  const requestGeneration = useRef(0);
  // Freshest signed session URL (token bound to the lease's expires_at). The
  // renewal timer keeps this current WITHOUT swapping the live stream's
  // sessionUrl (which would needlessly reconnect the desktop); the stream uses
  // it only when it has to reconnect after an unclean drop.
  const latestSessionUrlRef = useRef<string | null>(null);

  useEffect(() => {
    const generation = requestGeneration.current + 1;
    requestGeneration.current = generation;
    let cancelled = false;
    setStatus(null);
    setControl(null);
    setSessionUrl(null);
    latestSessionUrlRef.current = null;
    setError(null);
    setControlError(null);
    setControlBusy(false);
    async function load() {
      try {
        const displayStatus = await api.get<DisplayStatus>(`/workspaces/${workspaceId}/display`);
        if (cancelled || requestGeneration.current !== generation) return;
        setStatus(displayStatus);
        if (displayStatus.reason === "display_not_enabled") return;
        try {
          const displayControl = await api.get<DisplayControlStatus>(`/workspaces/${workspaceId}/display/control`);
          if (!cancelled && requestGeneration.current === generation) setControl(displayControl);
        } catch (err) {
          if (!cancelled && requestGeneration.current === generation) {
            setControl(null);
            setControlError("Display control unavailable");
          }
        }
      } catch (err) {
        if (!cancelled && requestGeneration.current === generation) setError("The display status could not be loaded.");
      }
    }
    load();
    return () => {
      cancelled = true;
    };
  }, [workspaceId]);

  // Acquire (or re-acquire) the display-control lease as the current holder.
  // Re-acquiring extends the 300s server-side lock AND returns a freshly-signed
  // session URL (token bound to the new expires_at). Used both to renew the
  // lease on a timer and to mint a non-stale token for each reconnect — a
  // cached URL can be past its ~300s expiry, which would make a reconnect 401.
  const reacquireSession = useCallback(async (): Promise<string | null> => {
    const generation = requestGeneration.current;
    try {
      const next = await api.post<DisplayControlStatus>(
        `/workspaces/${workspaceId}/display/control/acquire`,
        { controller: "user", ttl_seconds: 300 },
      );
      if (requestGeneration.current !== generation) return null;
      setControl(next);
      if (next.session_url) latestSessionUrlRef.current = next.session_url;
      return next.session_url ?? null;
    } catch {
      // Transient failure, or another holder took over: the live stream keeps
      // running on its existing connection; a reconnect re-evaluates control.
      return null;
    }
  }, [workspaceId]);

  // Renew the lease while we hold it. The lock is a 300s lease with no
  // server-side auto-renewal, so without this the control (and the session
  // token) silently expire mid-session — the user appears "kicked" every ~5
  // minutes. We renew well inside the TTL and do not touch the live stream.
  useEffect(() => {
    if (!sessionUrl) return;
    const timer = setInterval(() => {
      void reacquireSession();
    }, 120_000);
    return () => clearInterval(timer);
  }, [sessionUrl, reacquireSession]);

  const acquireControl = async () => {
    const generation = requestGeneration.current;
    const controlPath = `/workspaces/${workspaceId}/display/control`;
    setControlBusy(true);
    setControlError(null);
    try {
      const next = await api.post<DisplayControlStatus>(`${controlPath}/acquire`, {
        controller: "user",
        ttl_seconds: 300,
      });
      if (requestGeneration.current !== generation) return;
      setControl(next);
      setSessionUrl(next.session_url || null);
      latestSessionUrlRef.current = next.session_url || null;
    } catch (err) {
      if (requestGeneration.current !== generation) return;
      setControlError("Failed to take control");
      try {
        const latest = await api.get<DisplayControlStatus>(controlPath);
        if (requestGeneration.current !== generation) return;
        setControl(latest);
      } catch {
        if (requestGeneration.current !== generation) return;
        setControl(null);
      }
    } finally {
      if (requestGeneration.current === generation) setControlBusy(false);
    }
  };

  const releaseControl = async () => {
    const generation = requestGeneration.current;
    const controlPath = `/workspaces/${workspaceId}/display/control`;
    setControlBusy(true);
    setControlError(null);
    try {
      const next = await api.post<DisplayControlStatus>(`${controlPath}/release`, {});
      if (requestGeneration.current !== generation) return;
      setControl(next);
      setSessionUrl(null);
      latestSessionUrlRef.current = null;
    } catch (err) {
      if (requestGeneration.current !== generation) return;
      setControlError("Failed to release control");
      try {
        const latest = await api.get<DisplayControlStatus>(controlPath);
        if (requestGeneration.current !== generation) return;
        setControl(latest);
      } catch {
        if (requestGeneration.current !== generation) return;
        setControl(null);
      }
    } finally {
      if (requestGeneration.current === generation) setControlBusy(false);
    }
  };

  if (error) {
    return (
      <div className="p-5">
        <div className="rounded-lg border border-red-500/20 bg-red-950/20 p-4">
          <h3 className="text-sm font-medium text-red-200">Display status unavailable</h3>
          <p className="mt-2 text-[11px] leading-relaxed text-red-200/75">{error}</p>
        </div>
      </div>
    );
  }

  if (!status) {
    return (
      <div className="p-5">
        <div className="h-24 rounded-lg border border-line/40 bg-surface-sunken/30 motion-safe:animate-pulse" />
      </div>
    );
  }

  if (!status.available) {
    const isNotEnabled = status.reason === "display_not_enabled";
    return (
      <div className="flex min-h-full flex-col items-center justify-center bg-surface-sunken/30 p-8 text-center">
        <svg
          width="72"
          height="72"
          viewBox="0 0 72 72"
          fill="none"
          aria-hidden="true"
          className="mb-4 text-ink-mid"
        >
          <rect x="12" y="14" width="48" height="36" rx="4" stroke="currentColor" strokeWidth="2.5" opacity="0.65" />
          <path d="M28 58h16M36 50v8M16 16l40 40" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
        </svg>
        <h3 className="mb-1.5 text-sm font-medium text-ink">
          {isNotEnabled ? "Display is not enabled for this workspace." : "Display session is not ready."}
        </h3>
        <p className="max-w-xs text-[11px] leading-relaxed text-ink-mid">
          {isNotEnabled
            ? "Recreate this workspace with display enabled to view and take over its desktop."
            : "This workspace has display configuration, but the desktop session infrastructure is not configured yet."}
        </p>
        {!isNotEnabled && (
          <>
            <dl className="mt-5 grid grid-cols-2 gap-x-4 gap-y-2 text-left text-[11px]">
              <dt className="text-ink-mid">Mode</dt>
              <dd className="font-mono text-ink">{status.mode || "unknown"}</dd>
              <dt className="text-ink-mid">Status</dt>
              <dd className="font-mono text-ink">{status.status || "unknown"}</dd>
            </dl>
            <div className="mt-5 w-full max-w-xs border-t border-line/50 pt-4">
              {control ? (
                <div className="flex items-center justify-between gap-3 text-left">
                  <div className="min-w-0">
                    <p className="text-[11px] font-medium text-ink">
                      {control.controller === "none"
                        ? "No active controller"
                        : `Controlled by ${displayControlActorLabel(control)}`}
                    </p>
                    {control.expires_at && (
                      <p className="mt-1 truncate font-mono text-[10px] text-ink-mid">
                        Until {new Date(control.expires_at).toLocaleTimeString()}
                      </p>
                    )}
                    {controlError && <p className="mt-1 text-[10px] leading-snug text-red-200">{controlError}</p>}
                  </div>
                  {control.controller === "none" && (
                    <button
                      type="button"
                      onClick={acquireControl}
                      disabled={controlBusy}
                      className="h-8 shrink-0 rounded border border-line bg-surface px-3 text-[11px] font-medium text-ink hover:bg-surface-elevated disabled:cursor-not-allowed disabled:opacity-60"
                    >
                      Take control
                    </button>
                  )}
                </div>
              ) : (
                <div className="text-left">
                  {!controlError && (
                    <div className="h-8 rounded border border-line/40 bg-surface-sunken/30 motion-safe:animate-pulse" />
                  )}
                  {controlError && <p className="mt-2 text-[10px] leading-snug text-red-200">{controlError}</p>}
                </div>
              )}
            </div>
          </>
        )}
      </div>
    );
  }

  return (
    <div className="flex h-full min-h-[360px] flex-col bg-surface-sunken/30">
      <div className="flex items-center justify-between gap-3 border-b border-line/50 px-4 py-3">
        <div className="min-w-0">
          <h3 className="text-sm font-medium text-ink">Desktop</h3>
          <p className="mt-0.5 font-mono text-[10px] text-ink-mid">
            {status.mode || "desktop-control"} · {status.protocol || "display"}
          </p>
        </div>
        <DisplayControlBar
          control={control}
          controlBusy={controlBusy}
          controlError={controlError}
          hasSession={!!sessionUrl}
          onAcquire={acquireControl}
          onRelease={releaseControl}
        />
      </div>
      {sessionUrl ? (
        <DesktopStream
          sessionUrl={sessionUrl}
          latestSessionUrlRef={latestSessionUrlRef}
          reacquireSession={reacquireSession}
        />
      ) : (
        <div className="flex flex-1 items-center justify-center p-8 text-center">
          <div>
            <h3 className="mb-1.5 text-sm font-medium text-ink">Take control to open the desktop.</h3>
            <p className="max-w-xs text-[11px] leading-relaxed text-ink-mid">
              The display service is ready. Control access opens a short-lived desktop stream.
            </p>
          </div>
        </div>
      )}
    </div>
  );
}

function DisplayControlBar({
  control,
  controlBusy,
  controlError,
  hasSession,
  onAcquire,
  onRelease,
}: {
  control: DisplayControlStatus | null;
  controlBusy: boolean;
  controlError: string | null;
  hasSession: boolean;
  onAcquire: () => void;
  onRelease: () => void;
}) {
  const userControl = control?.controller === "user";
  const adminControl = userControl && control?.controlled_by === "admin-token";
  const canAcquireUserControl = control?.controller === "none" || (userControl && !hasSession);
  const canReleaseUserControl = adminControl || (userControl && hasSession);

  return (
    <div className="flex min-w-0 items-center gap-3">
      {control && (
        <div className="min-w-0 text-right">
          <p className="truncate text-[11px] font-medium text-ink">
            {control.controller === "none"
              ? "No active controller"
              : `Controlled by ${displayControlActorLabel(control)}`}
          </p>
          {control.expires_at && (
            <p className="mt-0.5 truncate font-mono text-[10px] text-ink-mid">
              Until {new Date(control.expires_at).toLocaleTimeString()}
            </p>
          )}
          {controlError && <p className="mt-0.5 text-[10px] text-red-200">{controlError}</p>}
        </div>
      )}
      {canAcquireUserControl && (
        <button
          type="button"
          onClick={onAcquire}
          disabled={controlBusy}
          className="h-8 shrink-0 rounded border border-line bg-surface px-3 text-[11px] font-medium text-ink hover:bg-surface-elevated disabled:cursor-not-allowed disabled:opacity-60"
        >
          Take control
        </button>
      )}
      {canReleaseUserControl && (
        <button
          type="button"
          onClick={onRelease}
          disabled={controlBusy}
          className="h-8 shrink-0 rounded border border-line bg-surface px-3 text-[11px] font-medium text-ink hover:bg-surface-elevated disabled:cursor-not-allowed disabled:opacity-60"
        >
          Release
        </button>
      )}
    </div>
  );
}

function DesktopStream({
  sessionUrl,
  latestSessionUrlRef,
  reacquireSession,
}: {
  sessionUrl: string;
  latestSessionUrlRef: { current: string | null };
  reacquireSession: () => Promise<string | null>;
}) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const rfbRef = useRef<RFB | null>(null);
  const [streamError, setStreamError] = useState<string | null>(null);
  const [clipboardStatus, setClipboardStatus] = useState<string | null>(null);
  const [remoteClipboardText, setRemoteClipboardText] = useState("");

  useEffect(() => {
    let cancelled = false;
    let rfb: RFB | null = null;
    let clipboardTimer: ReturnType<typeof setTimeout> | null = null;

    const setTemporaryClipboardStatus = (message: string) => {
      setClipboardStatus(message);
      if (clipboardTimer) clearTimeout(clipboardTimer);
      clipboardTimer = setTimeout(() => setClipboardStatus(null), 2500);
    };

    let attempts = 0;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    const maxAttempts = 10;

    async function connect(reacquire = false) {
      setStreamError(null);
      try {
        // On a reconnect, mint a fresh lease + token first — the original token
        // is only ~300s, so a cached URL can be expired and would 401. The
        // initial connect already holds a fresh token from acquireControl.
        if (reacquire) await reacquireSession();
        const mod = await import("@novnc/novnc");
        if (cancelled || !containerRef.current) return;
        const stream = displayWebSocketConnection(latestSessionUrlRef.current || sessionUrl);
        rfb = new mod.default(containerRef.current, stream.url, {
          wsProtocols: ["binary", `molecule-display-token.${stream.token}`],
        });
        rfbRef.current = rfb;
        rfb.scaleViewport = true;
        // Do NOT request a server-side resize: the workspace display runs a
        // fixed Xorg modeline and x11vnc rejects SetDesktopSize ("Resize is
        // administratively prohibited"), which spams the console on every
        // (re)connect. scaleViewport already fits the fixed framebuffer to the
        // container client-side, so we don't need the server to resize.
        rfb.resizeSession = false;
        rfb.focusOnClick = true;
        rfb.focus({ preventScroll: true });
        rfb.addEventListener("connect", () => {
          attempts = 0;
          if (!cancelled) setStreamError(null);
        });
        rfb.addEventListener("clipboard", (event: Event) => {
          const text = (event as CustomEvent<{ text?: string }>).detail?.text ?? "";
          if (!text) return;
          setRemoteClipboardText(text);
          void navigator.clipboard?.writeText(text)
            .then(() => setTemporaryClipboardStatus("Copied remote clipboard"))
            .catch(() => setTemporaryClipboardStatus("Remote clipboard ready"));
        });
        rfb.addEventListener("disconnect", (event: Event) => {
          const detail = (event as CustomEvent<{ clean?: boolean }>).detail;
          rfbRef.current = null;
          if (cancelled || detail?.clean) return;
          // Auto-reconnect after an unclean drop (idle/network blip, brief
          // agent hiccup); bounded backoff so a genuinely-dead session still
          // surfaces an error instead of looping forever.
          if (attempts < maxAttempts) {
            attempts += 1;
            setStreamError(`Reconnecting to desktop… (attempt ${attempts})`);
            retryTimer = setTimeout(() => {
              if (!cancelled) void connect(true);
            }, Math.min(1000 * attempts, 5000));
          } else {
            setStreamError("Desktop stream disconnected.");
          }
        });
      } catch {
        if (!cancelled) setStreamError("Desktop stream could not be opened.");
      }
    }

    connect();
    return () => {
      cancelled = true;
      if (retryTimer) clearTimeout(retryTimer);
      if (clipboardTimer) clearTimeout(clipboardTimer);
      rfbRef.current = null;
      rfb?.disconnect();
    };
  }, [sessionUrl, reacquireSession, latestSessionUrlRef]);

  useEffect(() => {
    const onPaste = (event: ClipboardEvent) => {
      if (!isDisplayEventTarget(containerRef.current, event.target)) return;
      const text = event.clipboardData?.getData("text/plain") ?? "";
      if (!text) return;
      event.preventDefault();
      rfbRef.current?.clipboardPasteFrom(text);
      rfbRef.current?.focus({ preventScroll: true });
      setClipboardStatus("Pasted to desktop");
    };
    window.addEventListener("paste", onPaste, true);
    return () => window.removeEventListener("paste", onPaste, true);
  }, []);

  const pasteLocalClipboard = async () => {
    try {
      const text = await navigator.clipboard?.readText();
      if (!text) {
        setClipboardStatus("Clipboard is empty");
        return;
      }
      rfbRef.current?.clipboardPasteFrom(text);
      rfbRef.current?.focus({ preventScroll: true });
      setClipboardStatus("Pasted to desktop");
    } catch {
      setClipboardStatus("Press Ctrl/Cmd+V while the desktop is focused");
    }
  };

  const copyRemoteClipboard = async () => {
    if (!remoteClipboardText) {
      setClipboardStatus("No remote clipboard yet");
      return;
    }
    try {
      await navigator.clipboard.writeText(remoteClipboardText);
      setClipboardStatus("Copied remote clipboard");
    } catch {
      setClipboardStatus("Browser blocked clipboard copy");
    }
  };

  return (
    <div
      data-display-stream="true"
      className="relative min-h-0 flex-1 bg-black"
      onMouseDown={() => rfbRef.current?.focus({ preventScroll: true })}
    >
      <div ref={containerRef} title="Workspace desktop" className="h-full w-full overflow-hidden bg-black" />
      <div className="absolute right-3 top-3 flex items-center gap-2">
        {clipboardStatus && (
          <span className="rounded border border-line/50 bg-black/80 px-2 py-1 text-[10px] text-white">
            {clipboardStatus}
          </span>
        )}
        <button
          type="button"
          onClick={pasteLocalClipboard}
          className="h-7 rounded border border-line/50 bg-black/75 px-2 text-[10px] font-medium text-white hover:bg-black"
        >
          Paste
        </button>
        <button
          type="button"
          onClick={copyRemoteClipboard}
          className="h-7 rounded border border-line/50 bg-black/75 px-2 text-[10px] font-medium text-white hover:bg-black disabled:cursor-not-allowed disabled:opacity-50"
          disabled={!remoteClipboardText}
        >
          Copy
        </button>
      </div>
      {streamError && (
        <div className="absolute inset-x-4 top-4 rounded border border-red-500/30 bg-red-950/80 px-3 py-2 text-[11px] text-red-100">
          {streamError}
        </div>
      )}
    </div>
  );
}

function isDisplayEventTarget(container: HTMLElement | null, target: EventTarget | null): boolean {
  if (!container) return false;
  if (target instanceof Node && container.contains(target)) return true;
  const active = document.activeElement;
  return active instanceof Node && container.contains(active);
}

function displayWebSocketConnection(sessionUrl: string): { url: string; token: string } {
  const url = new URL(sessionUrl, window.location.href);
  const token = new URLSearchParams(url.hash.replace(/^#/, "")).get("token") ?? "";
  if (!token) throw new Error("display session token missing");
  url.hash = "";
  url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return { url: url.toString(), token };
}

function displayControlActorLabel(control: DisplayControlStatus): string {
  if (control.controller === "agent") return "Agent";
  if (control.controlled_by === "admin-token") return "Admin";
  if (control.controlled_by?.startsWith("org-token:")) return "Automation";
  return "User";
}
