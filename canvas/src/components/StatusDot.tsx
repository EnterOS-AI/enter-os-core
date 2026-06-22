"use client";

import { STATUS_CONFIG, statusDotClass } from "@/lib/design-tokens";

export function StatusDot({
  status,
  size = "sm",
}: {
  status: string;
  size?: "sm" | "md";
}) {
  const sizeClass = size === "md" ? "w-2.5 h-2.5" : "w-2 h-2";
  const glowClass = STATUS_CONFIG[status]?.glow ?? "";
  return (
    <div
      className={`${sizeClass} rounded-full shrink-0 ${statusDotClass(status)} ${glowClass}`}
      aria-hidden="true"
      role="img"
    />
  );
}
