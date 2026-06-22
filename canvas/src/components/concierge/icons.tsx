/* Inline SVG icons lifted from the Org Concierge concept (molecule-concierge-v1).
   Stroke icons inherit currentColor; size comes from the CSS (svg{width/height}). */
import type { SVGProps } from "react";

const stroke = {
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.8,
  strokeLinecap: "round" as const,
  strokeLinejoin: "round" as const,
};

export const IcMolecule = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" {...p}>
    <circle cx="12" cy="5" r="2.4" fill="#fff" />
    <circle cx="5.5" cy="16" r="2.4" fill="#fff" opacity=".85" />
    <circle cx="18.5" cy="16" r="2.4" fill="#fff" opacity=".85" />
    <path d="M12 7.4L6 14.2M12 7.4L18 14.2M7.6 16h8.8" stroke="#fff" strokeWidth="1.4" strokeLinecap="round" />
  </svg>
);

export const IcChat = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" {...p}>
    <circle cx="12" cy="5" r="2.2" fill="#fff" />
    <circle cx="5.5" cy="16" r="2.2" fill="#fff" opacity=".85" />
    <circle cx="18.5" cy="16" r="2.2" fill="#fff" opacity=".85" />
    <path d="M12 7.2L6 14M12 7.2L18 14M7.6 16h8.8" stroke="#fff" strokeWidth="1.3" strokeLinecap="round" />
  </svg>
);

export const IcHome = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><path d="M3 10.5L12 3l9 7.5" /><path d="M5 9.5V20h14V9.5" /></svg>
);

export const IcOrgMap = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}>
    <rect x="8.5" y="3" width="7" height="6" rx="1.5" />
    <rect x="2.5" y="15" width="6.5" height="6" rx="1.5" />
    <rect x="15" y="15" width="6.5" height="6" rx="1.5" />
    <path d="M12 9v3M12 12H5.75v3M12 12h6.25v3" />
  </svg>
);

export const IcSettings = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}>
    <circle cx="12" cy="12" r="3" />
    <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1.03 1.56V21a2 2 0 1 1-4 0v-.09A1.7 1.7 0 0 0 9 19.4a1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.7 1.7 0 0 0 4.6 15a1.7 1.7 0 0 0-1.56-1.03H3a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.6 9a1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.7 1.7 0 0 0 9 4.6a1.7 1.7 0 0 0 1.03-1.56V3a2 2 0 1 1 4 0v.09A1.7 1.7 0 0 0 15 4.6a1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.7 1.7 0 0 0 19.4 9c.13.31.4.55.73.66" />
  </svg>
);

export const IcSearch = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" {...p}><circle cx="11" cy="11" r="7" /><path d="M20 20l-3.5-3.5" /></svg>
);

export const IcBell = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><path d="M18 8a6 6 0 1 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" /><path d="M13.7 21a2 2 0 0 1-3.4 0" /></svg>
);

export const IcSun = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><circle cx="12" cy="12" r="4.2" /><path d="M12 2v2.5M12 19.5V22M2 12h2.5M19.5 12H22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M19.1 4.9l-1.8 1.8M6.7 17.3l-1.8 1.8" /></svg>
);

export const IcMoon = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" /></svg>
);

export const IcChevDown = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" {...p}><path d="M6 9l6 6 6-6" /></svg>
);

export const IcCaret = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" {...p}><path d="M9 6l6 6-6 6" /></svg>
);

export const IcQueue = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" {...p}><path d="M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01" /></svg>
);

export const IcCheck = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" {...p}><path d="M20 6L9 17l-5-5" /></svg>
);

export const IcSchedule = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><rect x="3.5" y="4.5" width="17" height="16" rx="2.5" /><path d="M3.5 9h17M8 3v3M16 3v3" /></svg>
);

export const IcWorkspace = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><rect x="3.5" y="3.5" width="7" height="7" rx="1.5" /><rect x="13.5" y="13.5" width="7" height="7" rx="1.5" /><path d="M13.5 7h7M7 13.5v7" /></svg>
);

export const IcWarn = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><path d="M12 9v4M12 17h.01" /><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z" /></svg>
);

export const IcSend = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" {...p}><path d="M5 12h14M13 6l6 6-6 6" /></svg>
);

export const IcHistory = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><path d="M3 12a9 9 0 1 0 3-6.7L3 8" /><path d="M3 4v4h4M12 8v4l3 2" /></svg>
);

export const IcDots = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" fill="currentColor" {...p}><circle cx="5" cy="12" r="1.6" /><circle cx="12" cy="12" r="1.6" /><circle cx="19" cy="12" r="1.6" /></svg>
);

export const IcClock = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><path d="M12 7v5l3 2" /><circle cx="12" cy="12" r="9" /></svg>
);

export const IcTrash = (p: SVGProps<SVGSVGElement>) => (
  <svg viewBox="0 0 24 24" {...stroke} {...p}><path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6M10 11v5M14 11v5" /></svg>
);
