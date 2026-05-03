"use client";

import React from "react";

interface ErrorBoundaryProps {
  children: React.ReactNode;
}

interface ErrorBoundaryState {
  hasError: boolean;
  error: Error | null;
}

export class ErrorBoundary extends React.Component<
  ErrorBoundaryProps,
  ErrorBoundaryState
> {
  constructor(props: ErrorBoundaryProps) {
    super(props);
    this.state = { hasError: false, error: null };
  }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, errorInfo: React.ErrorInfo): void {
    console.error("ErrorBoundary caught an error:", error, errorInfo.componentStack);
  }

  handleReload = () => {
    window.location.reload();
  };

  handleReport = () => {
    const errorDetails = {
      message: this.state.error?.message ?? "Unknown error",
      stack: this.state.error?.stack ?? "N/A",
      timestamp: new Date().toISOString(),
      url: window.location.href,
    };
    // Log the full report to console for collection by monitoring tools
    console.error("Error Report:", JSON.stringify(errorDetails, null, 2));
    // Copy error info to clipboard for manual reporting (button click is its
    // own affordance — no native alert needed). On clipboard failure the
    // console.error above still surfaces the report.
    void navigator.clipboard?.writeText(JSON.stringify(errorDetails, null, 2))
      .catch((e) => console.warn("clipboard write failed:", e));
  };

  render() {
    if (this.state.hasError) {
      return (
        <div className="fixed inset-0 flex items-center justify-center bg-surface z-50">
          <div className="max-w-md rounded-2xl border border-red-500/30 bg-surface-sunken/90 px-8 py-8 text-center shadow-2xl shadow-black/40">
            <div className="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-full bg-red-500/10 border border-red-500/30">
              <svg
                width="24"
                height="24"
                viewBox="0 0 24 24"
                fill="none"
                stroke="#ef4444"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <circle cx="12" cy="12" r="10" />
                <line x1="12" y1="8" x2="12" y2="12" />
                <line x1="12" y1="16" x2="12.01" y2="16" />
              </svg>
            </div>
            <h2 className="text-lg font-semibold text-ink mb-2">
              Something went wrong
            </h2>
            <p className="text-sm text-ink-mid mb-1">
              An unexpected error occurred while rendering the application.
            </p>
            <p className="text-xs text-bad/80 mb-6 font-mono break-all">
              {this.state.error?.message ?? "Unknown error"}
            </p>
            <div className="flex items-center justify-center gap-3">
              <button
                type="button"
                onClick={this.handleReload}
                className="rounded-lg bg-accent-strong hover:bg-accent px-5 py-2 text-sm font-medium text-white transition-colors"
              >
                Reload
              </button>
              <a
                href="#report"
                onClick={(e) => {
                  e.preventDefault();
                  this.handleReport();
                }}
                className="rounded-lg border border-line hover:border-line px-5 py-2 text-sm font-medium text-ink-mid hover:text-ink transition-colors"
              >
                Report
              </a>
            </div>
          </div>
        </div>
      );
    }

    return this.props.children;
  }
}
