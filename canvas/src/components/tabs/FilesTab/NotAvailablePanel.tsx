"use client";

/**
 * NotAvailablePanel — full-tab placeholder for runtimes whose filesystem
 * the platform doesn't own (today: runtime === "external").
 *
 * Pre-fix the FilesTab tried to GET /workspaces/<id>/files for these
 * workspaces. The platform answered with [] (no rows in workspace_files
 * for an external workspace by definition), but the canvas rendered
 * "0 files / No config files yet" which reads identically to the SaaS
 * empty-listing bug fixed in PR-A. Showing an explicit placeholder
 * makes the absence intentional and routes the user toward the
 * supported surface (Chat) for these workspaces.
 *
 * Mirrors the same affordance TerminalTab adopted for runtimes without
 * a TTY in PR #2830 — uniform "feature-not-applicable" UX across tabs.
 */
export function NotAvailablePanel({ runtime }: { runtime: string }) {
  return (
    <div className="flex flex-col items-center justify-center h-full p-8 text-center bg-surface-sunken/30">
      {/* Folder-with-slash icon. Custom inline SVG so we don't depend
          on an icon set being present at canvas build-time (matches
          TerminalTab's NotAvailablePanel pattern). */}
      <svg
        width="72"
        height="72"
        viewBox="0 0 72 72"
        fill="none"
        aria-hidden="true"
        className="text-ink-soft mb-4"
      >
        {/* Folder body */}
        <path
          d="M10 22 L10 56 a4 4 0 0 0 4 4 L58 60 a4 4 0 0 0 4 -4 L62 26 a4 4 0 0 0 -4 -4 L34 22 L28 16 L14 16 a4 4 0 0 0 -4 4 Z"
          stroke="currentColor"
          strokeWidth="2.5"
          strokeLinejoin="round"
          fill="none"
          opacity="0.6"
        />
        {/* Diagonal cancel slash */}
        <path
          d="M14 14 L58 58"
          stroke="currentColor"
          strokeWidth="3"
          strokeLinecap="round"
        />
      </svg>
      <h3 className="text-sm font-medium text-ink mb-1.5">Files not available</h3>
      <p className="text-[11px] text-ink-soft max-w-xs leading-relaxed">
        This workspace runs the{" "}
        <span className="font-mono text-ink-mid">{runtime}</span> runtime,
        whose filesystem isn't owned by the platform. Use the Chat tab to
        interact with the agent directly.
      </p>
    </div>
  );
}
