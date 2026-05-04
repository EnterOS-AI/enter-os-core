"use client";

import { useEffect, useRef, useState, useCallback } from "react";

interface Props {
  workspaceId: string;
}

import { deriveWsBaseUrl } from "@/lib/ws-url";

const WS_URL = deriveWsBaseUrl();

export function TerminalTab({ workspaceId }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<{ dispose: () => void } | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const observerRef = useRef<ResizeObserver | null>(null);
  const [status, setStatus] = useState<"connecting" | "connected" | "disconnected" | "error">("disconnected");
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [connectKey, setConnectKey] = useState(0);

  const cleanup = useCallback(() => {
    wsRef.current?.close();
    wsRef.current = null;
    observerRef.current?.disconnect();
    observerRef.current = null;
    termRef.current?.dispose();
    termRef.current = null;
  }, []);

  useEffect(() => {
    if (!containerRef.current) return;

    let cancelled = false;

    async function init() {
      const el = containerRef.current;
      if (!el || cancelled) return;

      const { Terminal } = await import("xterm");
      const { FitAddon } = await import("@xterm/addon-fit");
      if (cancelled) return;

      const fitAddon = new FitAddon();
      const term = new Terminal({
        theme: {
          background: "#18181b",
          foreground: "#e4e4e7",
          cursor: "#3b82f6",
          selectionBackground: "#3b82f644",
        },
        fontFamily: "JetBrains Mono, Menlo, Monaco, monospace",
        fontSize: 12,
        cursorBlink: true,
      });

      term.loadAddon(fitAddon);
      term.open(el);
      fitAddon.fit();
      termRef.current = term;

      // Connect WebSocket
      setStatus("connecting");
      const wsUrl = `${WS_URL}/workspaces/${workspaceId}/terminal`;
      const socket = new WebSocket(wsUrl);
      wsRef.current = socket;
      socket.binaryType = "arraybuffer";

      socket.onopen = () => {
        if (cancelled) return;
        setStatus("connected");
        setErrorMsg(null);
        term.writeln("\x1b[32mConnected to workspace shell\x1b[0m");
        term.writeln("");
        fitAddon.fit();
      };

      socket.onmessage = (event) => {
        if (event.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(event.data));
        } else {
          term.write(event.data);
        }
      };

      socket.onclose = () => {
        if (cancelled) return;
        setStatus("disconnected");
        term.writeln("");
        term.writeln("\x1b[33mSession ended\x1b[0m");
      };

      socket.onerror = () => {
        if (cancelled) return;
        setStatus("error");
        setErrorMsg("Failed to connect — is the workspace container running?");
      };

      term.onData((data: string) => {
        if (socket.readyState === WebSocket.OPEN) {
          socket.send(data);
        }
      });

      const observer = new ResizeObserver(() => fitAddon.fit());
      observer.observe(el);
      observerRef.current = observer;
    }

    init();

    return () => {
      cancelled = true;
      cleanup();
    };
  }, [workspaceId, connectKey, cleanup]);

  const reconnect = useCallback(() => {
    cleanup();
    setErrorMsg(null);
    setConnectKey((k) => k + 1);
  }, [cleanup]);

  return (
    <div className="flex flex-col h-full">
      {/* Status bar — role="status" so connection state changes are announced politely.
          Terminal body stays dark unconditionally (Canvas v4 design rule), but the
          chrome wrapping it now uses semantic status colors so the dot/text stay
          readable in both themes. */}
      <div role="status" aria-live="polite" className="flex items-center justify-between px-3 py-1.5 border-b border-zinc-700 bg-zinc-800/50">
        <div className="flex items-center gap-2">
          <div className={`w-2 h-2 rounded-full ${
            status === "connected" ? "bg-good" :
            status === "connecting" ? "bg-warm motion-safe:animate-pulse" :
            status === "error" ? "bg-bad" : "bg-ink-soft"
          }`} />
          <span className="text-[10px] text-zinc-300">
            {status === "connected" ? "Shell active" :
             status === "connecting" ? "Connecting..." :
             status === "error" ? "Connection failed" : "Disconnected"}
          </span>
        </div>
        {(status === "disconnected" || status === "error") && (
          <button
            type="button"
            onClick={reconnect}
            // Accent over hardcoded blue. text-accent + hover-strong stays
            // readable on the dark terminal chrome and matches the rest
            // of the canvas semantic palette. Focus-visible ring added so
            // keyboard users see where focus lands on a recovery button.
            className="text-[10px] text-accent hover:text-accent-strong rounded-sm px-1 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
          >
            Reconnect
          </button>
        )}
      </div>

      {/* Error message — role="alert" announces immediately via assertive live region */}
      {errorMsg && (
        <div role="alert" className="mx-3 mt-2 px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
          {errorMsg}
        </div>
      )}

      {/* Terminal */}
      <div ref={containerRef} className="flex-1 p-1" />
    </div>
  );
}
