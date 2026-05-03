"use client";

import { useEffect, useState } from "react";
import { PLATFORM_URL } from "@/lib/api";

// TermsGate blocks the page it wraps until the user has accepted the
// current terms version. Fetches /cp/auth/terms-status on mount; if
// the server says accepted=false it renders a modal over the children
// instead of hiding them entirely — that way the /orgs list is still
// visible behind the gate so the user understands what they're
// agreeing to touch.
//
// The server is the source of truth; this component is a UX
// convenience. Org-mutating endpoints should (and do) also enforce
// ToS via their own DB check so a power-user calling curl can't
// bypass the gate.
export function TermsGate({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<"loading" | "accepted" | "pending" | "error">("loading");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await fetch(`${PLATFORM_URL}/cp/auth/terms-status`, {
          credentials: "include",
          signal: AbortSignal.timeout(10_000),
        });
        if (cancelled) return;
        if (res.status === 401) {
          // Not signed in — the page this wraps handles redirect to login.
          // Fall through to "accepted" so we don't double-gate anonymous.
          setStatus("accepted");
          return;
        }
        if (!res.ok) {
          setStatus("error");
          setError(`terms-status: ${res.status}`);
          return;
        }
        const body = (await res.json()) as { accepted?: boolean };
        setStatus(body.accepted ? "accepted" : "pending");
      } catch (err) {
        if (!cancelled) {
          setStatus("error");
          setError(err instanceof Error ? err.message : String(err));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const accept = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const res = await fetch(`${PLATFORM_URL}/cp/auth/accept-terms`, {
        method: "POST",
        credentials: "include",
        signal: AbortSignal.timeout(10_000),
      });
      if (!res.ok) {
        const text = await res.text();
        throw new Error(`${res.status}: ${text}`);
      }
      setStatus("accepted");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSubmitting(false);
    }
  };

  return (
    <>
      {children}
      {status === "pending" && (
        <div aria-hidden="true" className="fixed inset-0 z-50 flex items-center justify-center bg-surface/80 backdrop-blur-sm">
          <div
            role="dialog"
            aria-modal="true"
            aria-labelledby="terms-dialog-title"
            className="mx-4 max-w-lg rounded-lg border border-line bg-surface-sunken p-6 shadow-xl"
          >
            <h2 id="terms-dialog-title" className="text-lg font-semibold text-ink">Terms &amp; conditions</h2>
            <p className="mt-3 text-sm text-ink-mid">
              Before you create an organization, please review our{" "}
              <a href="/legal/terms" className="text-sky-400 underline" target="_blank" rel="noreferrer">
                Terms of Service
              </a>{" "}
              and{" "}
              <a href="/legal/privacy" className="text-sky-400 underline" target="_blank" rel="noreferrer">
                Privacy Policy
              </a>
              . Click agree to continue.
            </p>
            <p className="mt-3 text-xs text-ink-soft">
              By agreeing you acknowledge that workspace data is stored in AWS us-east-2 (Ohio, United States).
            </p>
            {error && <p role="alert" className="mt-3 text-sm text-bad">{error}</p>}
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={accept}
                disabled={submitting}
                className="rounded bg-emerald-600 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-500 disabled:opacity-50"
              >
                {submitting ? "Saving…" : "I agree"}
              </button>
            </div>
          </div>
        </div>
      )}
      {status === "error" && (
        <div role="alert" className="fixed bottom-4 left-4 right-4 mx-auto max-w-md rounded border border-red-800 bg-red-950 p-3 text-sm text-red-200">
          Couldn&apos;t check terms status: {error ?? "unknown error"}
        </div>
      )}
    </>
  );
}
