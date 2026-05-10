"use client";

// Mobile primitives — StatusDot, TierChip, Chip, Icons, SectionLabel.
// Ports shared.jsx 1:1 from the design handoff; React + TypeScript flavor.

import type { CSSProperties, ReactNode, SVGProps } from "react";
import {
  MOBILE_FONT_MONO,
  type MobilePalette,
  type MobileStatus,
  usePalette,
} from "./palette";

type TierCode = "T1" | "T2" | "T3" | "T4";

export function StatusDot({
  status = "online",
  size = 8,
  dark = false,
  halo = true,
}: {
  status?: MobileStatus;
  size?: number;
  dark?: boolean;
  halo?: boolean;
}) {
  const p = usePalette(dark);
  const c: string = (p as unknown as Record<string, string>)[status] ?? p.online;
  return (
    <span
      style={{
        display: "inline-block",
        width: size,
        height: size,
        borderRadius: 999,
        background: c,
        flexShrink: 0,
        boxShadow: halo ? `0 0 0 ${Math.max(2, size * 0.45)}px ${c}26` : "none",
      }}
    />
  );
}

export function TierChip({
  tier = "T2",
  dark = false,
  size = "sm",
}: {
  tier?: TierCode;
  dark?: boolean;
  size?: "sm" | "lg";
}) {
  const p = usePalette(dark);
  const map: Record<TierCode, { bg: string; ink: string; br: string }> = {
    T1: { bg: p.t1Bg, ink: p.t1Ink, br: p.t1Br },
    T2: { bg: p.t2Bg, ink: p.t2Ink, br: p.t2Br },
    T3: { bg: p.t3Bg, ink: p.t3Ink, br: p.t3Br },
    T4: { bg: p.t4Bg, ink: p.t4Ink, br: p.t4Br },
  };
  const { bg, ink, br } = map[tier];
  const dim = size === "lg" ? { w: 32, h: 22, fs: 11 } : { w: 26, h: 19, fs: 10 };
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        justifyContent: "center",
        width: dim.w,
        height: dim.h,
        borderRadius: 5,
        background: bg,
        color: ink,
        border: `0.5px solid ${br}`,
        fontFamily: MOBILE_FONT_MONO,
        fontSize: dim.fs,
        fontWeight: 600,
        letterSpacing: "0.02em",
        flexShrink: 0,
      }}
    >
      {tier}
    </span>
  );
}

export function Chip({
  label,
  value,
  accent,
  dark = false,
  soft = false,
}: {
  label?: string;
  value: ReactNode;
  accent?: string;
  dark?: boolean;
  soft?: boolean;
}) {
  const p = usePalette(dark);
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        padding: "4px 9px",
        borderRadius: 999,
        background: soft
          ? `${accent ?? p.accent}1a`
          : dark
            ? "#2a2823"
            : "#f0ede5",
        border: `0.5px solid ${dark ? "rgba(255,255,255,0.06)" : "rgba(0,0,0,0.05)"}`,
        fontSize: 11,
        fontFamily: MOBILE_FONT_MONO,
        color: p.text2,
        letterSpacing: "0.02em",
      }}
    >
      {label && (
        <span style={{ textTransform: "uppercase", fontSize: 9.5, opacity: 0.7 }}>{label}</span>
      )}
      <span style={{ color: accent ?? p.text, fontWeight: 600 }}>{value}</span>
    </span>
  );
}

// ── icons (stroke-based, 20×20 viewBox) ───────────────────────
type IcoOpts = { stroke?: string; size?: number; fill?: string; sw?: number };
const ico = (
  paths: ReactNode,
  { stroke = "currentColor", size = 18, fill = "none", sw = 1.6 }: IcoOpts = {},
) => {
  const props: SVGProps<SVGSVGElement> = {
    width: size,
    height: size,
    viewBox: "0 0 20 20",
    fill,
    stroke,
    strokeWidth: sw,
    strokeLinecap: "round",
    strokeLinejoin: "round",
  };
  return <svg {...props}>{paths}</svg>;
};

export const Icons = {
  graph: (o?: IcoOpts) =>
    ico(
      <>
        <circle cx="5" cy="5" r="2" />
        <circle cx="15" cy="5" r="2" />
        <circle cx="10" cy="15" r="2" />
        <path d="M6.4 6.5l2.7 7M13.6 6.5l-2.7 7" />
      </>,
      o,
    ),
  list: (o?: IcoOpts) =>
    ico(
      <>
        <path d="M6 5h10M6 10h10M6 15h10" />
        <circle cx="3.5" cy="5" r="0.6" fill="currentColor" />
        <circle cx="3.5" cy="10" r="0.6" fill="currentColor" />
        <circle cx="3.5" cy="15" r="0.6" fill="currentColor" />
      </>,
      o,
    ),
  search: (o?: IcoOpts) =>
    ico(
      <>
        <circle cx="9" cy="9" r="5" />
        <path d="M13 13l4 4" />
      </>,
      o,
    ),
  plus: (o?: IcoOpts) => ico(<path d="M10 4v12M4 10h12" />, o),
  bell: (o?: IcoOpts) =>
    ico(
      <>
        <path d="M5 8a5 5 0 0 1 10 0v4l1.5 2H3.5L5 12V8z" />
        <path d="M8.5 16a1.5 1.5 0 0 0 3 0" />
      </>,
      o,
    ),
  chat: (o?: IcoOpts) =>
    ico(
      <path d="M4 5h12a1.5 1.5 0 0 1 1.5 1.5v6A1.5 1.5 0 0 1 16 14h-3l-3 3v-3H4a1.5 1.5 0 0 1-1.5-1.5v-6A1.5 1.5 0 0 1 4 5z" />,
      o,
    ),
  send: (o?: IcoOpts) =>
    ico(<path d="M3 10l14-6-5 14-3-6-6-2z" fill="currentColor" />, { ...o, sw: 1 }),
  attach: (o?: IcoOpts) =>
    ico(
      <path d="M14 6.5L7.5 13a2.5 2.5 0 0 0 3.5 3.5l7-7a4 4 0 0 0-5.6-5.6L4.8 11A6 6 0 0 0 13.3 19.5" />,
      o,
    ),
  back: (o?: IcoOpts) => ico(<path d="M12.5 4l-6 6 6 6" />, o),
  more: (o?: IcoOpts) =>
    ico(
      <>
        <circle cx="5" cy="10" r="1.2" fill="currentColor" />
        <circle cx="10" cy="10" r="1.2" fill="currentColor" />
        <circle cx="15" cy="10" r="1.2" fill="currentColor" />
      </>,
      o,
    ),
  filter: (o?: IcoOpts) => ico(<path d="M3 5h14M5 10h10M8 15h4" />, o),
  user: (o?: IcoOpts) =>
    ico(
      <>
        <circle cx="10" cy="7" r="3" />
        <path d="M3.5 17a6.5 6.5 0 0 1 13 0" />
      </>,
      o,
    ),
  settings: (o?: IcoOpts) =>
    ico(
      <>
        <circle cx="10" cy="10" r="2.2" />
        <path d="M10 2.5v2M10 15.5v2M2.5 10h2M15.5 10h2M4.7 4.7l1.4 1.4M13.9 13.9l1.4 1.4M4.7 15.3l1.4-1.4M13.9 6.1l1.4-1.4" />
      </>,
      o,
    ),
  pulse: (o?: IcoOpts) => ico(<path d="M2 10h3l2-5 3 10 2-7 2 4 4-2" />, o),
  close: (o?: IcoOpts) => ico(<path d="M5 5l10 10M15 5L5 15" />, o),
  zap: (o?: IcoOpts) => ico(<path d="M11 2l-6 9h4l-1 7 6-9h-4l1-7z" />, o),
  check: (o?: IcoOpts) => ico(<path d="M4 10l4 4 8-9" />, o),
  swatch: (o?: IcoOpts) =>
    ico(
      <>
        <rect x="3" y="3" width="6" height="6" rx="1" />
        <rect x="11" y="3" width="6" height="6" rx="1" />
        <rect x="3" y="11" width="6" height="6" rx="1" />
        <circle cx="14" cy="14" r="3.2" />
      </>,
      o,
    ),
};

export function SectionLabel({
  children,
  dark = false,
  right,
  style,
}: {
  children: ReactNode;
  dark?: boolean;
  right?: ReactNode;
  style?: CSSProperties;
}) {
  const p = usePalette(dark);
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        justifyContent: "space-between",
        padding: "14px 20px 6px",
        fontFamily: MOBILE_FONT_MONO,
        fontSize: 10.5,
        letterSpacing: "0.12em",
        textTransform: "uppercase",
        color: p.text3,
        fontWeight: 600,
        ...style,
      }}
    >
      <span>{children}</span>
      {right}
    </div>
  );
}

// Convenience: avoid repeating the (palette, dark) plumbing in screens
// that only need the palette object.
export function withPalette<T>(dark: boolean, fn: (p: MobilePalette) => T): T {
  return fn(usePalette(dark));
}
