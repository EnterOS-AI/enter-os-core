"use client";

import { useEffect, useState } from "react";
import { isSaaSTenant } from "@/lib/tenant";

const STORAGE_KEY = "molecule_cookie_consent";

// Three states, not two: "necessary-only" is distinct from "rejected"
// under GDPR/ePrivacy because the banner is supposed to let the user
// accept *some* cookies (functional, analytics) while still rejecting
// others. We keep the schema simple and offer just "accepted" (all)
// vs "rejected" (necessary only) for now — a future version can add
// per-category toggles if we ever ship analytics tracking.
export type ConsentDecision = "accepted" | "rejected";

interface StoredConsent {
  decision: ConsentDecision;
  decidedAt: string; // ISO-8601 UTC — makes audit logs unambiguous
  version: number;   // bump when the cookie policy changes materially
}

// Current cookie-policy version. Bump this when we add a new cookie
// category or change data-sharing scope; the banner will re-prompt
// every user whose stored decision is on an older version.
const CURRENT_VERSION = 1;

// getStoredConsent reads localStorage and returns null when either no
// decision exists OR the stored version is older than the current
// policy. Safe to call during render — guarded for SSR where window is
// undefined.
function getStoredConsent(): StoredConsent | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as StoredConsent;
    if (parsed.version !== CURRENT_VERSION) return null;
    return parsed;
  } catch {
    // Malformed JSON or localStorage blocked — treat as "no decision"
    // so the banner re-prompts. Better than swallowing the error and
    // leaving the user unable to recover.
    return null;
  }
}

// storeConsent persists a decision plus the current policy version so
// we know when to re-prompt. Failures are swallowed — if localStorage
// is blocked (private mode, quota) the banner will re-appear on next
// visit, which is the safer fallback than a runtime error.
function storeConsent(decision: ConsentDecision): void {
  try {
    const record: StoredConsent = {
      decision,
      decidedAt: new Date().toISOString(),
      version: CURRENT_VERSION,
    };
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(record));
  } catch {
    // intentional no-op
  }
}

// CookieConsent renders a dismissible footer banner that blocks nothing
// but visually prompts for a decision. Returns null after a decision is
// recorded so it doesn't waste vertical space for returning users.
//
// Privacy-preserving default: no cookies beyond strictly-necessary ones
// (session auth) are set until the user clicks Accept. Reject + dismiss
// both record "rejected" so we don't re-prompt until the next policy
// version bump.
export function CookieConsent() {
  const [visible, setVisible] = useState(false);

  // Read persisted decision on mount. useState's initialState can't run
  // on first render because localStorage is SSR-unsafe — defer to
  // useEffect so the initial HTML is identical to the server snapshot.
  //
  // The banner is SaaS-only: it carries a link to the hosted
  // privacy policy (moleculesai.app/legal/privacy) and presumes
  // GDPR/ePrivacy obligations that only apply to the hosted offering.
  // Self-hosted / local-dev / Vercel-preview hosts get no banner —
  // matches the `isSaaSTenant()` convention used by AuthGate and
  // the tier picker.
  useEffect(() => {
    if (!isSaaSTenant()) {
      setVisible(false);
      return;
    }
    setVisible(getStoredConsent() === null);
  }, []);

  if (!visible) return null;

  const decide = (decision: ConsentDecision) => {
    storeConsent(decision);
    setVisible(false);
  };

  return (
    // role="region" + aria-label, NOT role="dialog" + aria-modal. The
    // banner is informational — it never blocks the page, never traps
    // focus, and the user can keep using the canvas while it's up.
    // Claiming aria-modal="true" without a focus trap is genuinely
    // harmful for screen-reader users: they get told the rest of the
    // page is inert, jump into the banner, and then can't escape.
    // Region semantics let assistive tech navigate around it normally.
    // (Also: forcing a modal cookie banner would be a dark pattern —
    // GDPR explicitly discourages it.)
    <section
      role="region"
      aria-labelledby="cookie-consent-title"
      aria-describedby="cookie-consent-body"
      className="fixed bottom-0 left-0 right-0 z-[9999] border-t border-line bg-surface/95 backdrop-blur-sm p-4 shadow-[0_-4px_12px_rgba(0,0,0,0.4)]"
    >
      <div className="mx-auto flex max-w-5xl flex-col gap-3 md:flex-row md:items-center md:justify-between">
        <div className="text-sm text-ink-mid">
          <p id="cookie-consent-title" className="font-medium text-ink">
            Cookies &amp; your privacy
          </p>
          <p id="cookie-consent-body" className="mt-1 text-ink-mid">
            We use strictly-necessary cookies for authentication and session
            continuity. Accept to also allow optional functional cookies that
            improve your canvas experience (layout preferences, recent
            workspaces). See our{" "}
            <a
              href="https://moleculesai.app/legal/privacy"
              className="text-accent underline underline-offset-2 hover:text-accent-strong focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 rounded-sm"
              target="_blank"
              rel="noreferrer"
            >
              privacy policy
            </a>{" "}
            for details.
          </p>
        </div>
        <div className="flex gap-2 md:shrink-0">
          <button
            type="button"
            onClick={() => decide("rejected")}
            className="rounded border border-line bg-surface-sunken px-4 py-2 text-sm text-ink hover:bg-surface-card focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
          >
            Necessary only
          </button>
          <button
            type="button"
            onClick={() => decide("accepted")}
            className="rounded border border-accent bg-accent-strong px-4 py-2 text-sm font-medium text-white hover:bg-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
          >
            Accept all
          </button>
        </div>
      </div>
    </section>
  );
}

// hasConsent is a helper for feature code that needs to check whether
// optional cookies are allowed. Returns false under SSR or when no
// decision is on file, which matches the banner's privacy-preserving
// default ("assume no consent until proven otherwise").
export function hasConsent(): boolean {
  const stored = getStoredConsent();
  return stored?.decision === "accepted";
}
