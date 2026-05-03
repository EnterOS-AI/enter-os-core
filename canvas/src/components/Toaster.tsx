"use client";

import { useEffect, useState } from "react";

interface Toast {
  id: string;
  message: string;
  type: "success" | "error" | "info";
}

let addToastFn: ((message: string, type?: Toast["type"]) => void) | null = null;

/** Call from anywhere to show a toast */
export function showToast(message: string, type: Toast["type"] = "info") {
  addToastFn?.(message, type);
}

export function Toaster() {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const dismiss = (id: string) =>
    setToasts((prev) => prev.filter((t) => t.id !== id));

  useEffect(() => {
    addToastFn = (message, type = "info") => {
      const id = Math.random().toString(36).slice(2);
      setToasts((prev) => [...prev.slice(-4), { id, message, type }]);
      // Errors persist until the user explicitly dismisses them.
      // Success / info auto-expire after 4 s.
      if (type !== "error") {
        setTimeout(() => {
          setToasts((prev) => prev.filter((t) => t.id !== id));
        }, 4000);
      }
    };
    return () => {
      addToastFn = null;
    };
  }, []);

  const toastCls = (type: Toast["type"]) =>
    `flex items-center gap-2 pl-4 pr-2 py-2.5 rounded-xl shadow-2xl shadow-black/40 text-sm backdrop-blur-md animate-in slide-in-from-bottom duration-200 ${
      type === "success"
        ? "bg-emerald-950/90 border border-emerald-700/40 text-emerald-200"
        : type === "error"
        ? "bg-red-950/90 border border-red-700/40 text-red-200"
        : "bg-surface-sunken/90 border border-line/40 text-ink"
    }`;

  const pos =
    "fixed bottom-16 left-1/2 -translate-x-1/2 z-[80] flex flex-col gap-2 items-center";

  return (
    <>
      {/*
       * Polite live region — success & info notifications.
       * Always rendered so screen readers register it before any toast fires.
       */}
      <div role="status" aria-live="polite" aria-atomic="false" className={pos}>
        {toasts
          .filter((t) => t.type !== "error")
          .map((toast) => (
            <div key={toast.id} className={toastCls(toast.type)}>
              <span>{toast.message}</span>
              <button
                type="button"
                onClick={() => dismiss(toast.id)}
                aria-label="Dismiss notification"
                className="ml-1 p-1 rounded hover:bg-surface-card/50 transition-colors opacity-70 hover:opacity-100 shrink-0"
              >
                ×
              </button>
            </div>
          ))}
      </div>

      {/*
       * Assertive live region — errors only.
       * aria-live="assertive" interrupts the screen reader immediately.
       * Errors never auto-expire; user must dismiss via the × button.
       */}
      <div
        role="alert"
        aria-live="assertive"
        aria-atomic="false"
        className={pos}
      >
        {toasts
          .filter((t) => t.type === "error")
          .map((toast) => (
            <div key={toast.id} className={toastCls(toast.type)}>
              <span>{toast.message}</span>
              <button
                type="button"
                onClick={() => dismiss(toast.id)}
                aria-label="Dismiss notification"
                className="ml-1 p-1 rounded hover:bg-surface-card/50 transition-colors opacity-70 hover:opacity-100 shrink-0"
              >
                ×
              </button>
            </div>
          ))}
      </div>
    </>
  );
}
