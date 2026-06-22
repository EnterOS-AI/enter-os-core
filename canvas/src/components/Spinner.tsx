"use client";

const SIZES = { sm: "w-3 h-3", md: "w-4 h-4", lg: "w-5 h-5" } as const;

export function Spinner({ size = "md" }: { size?: keyof typeof SIZES }) {
  return (
    <svg className={`${SIZES[size]} motion-safe:animate-spin`} viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="2" opacity="0.25" />
      <path d="M12 2a10 10 0 0 1 10 10" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
}
