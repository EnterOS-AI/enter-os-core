"use client";

// "Me" tab — the prototype design didn't ship a Me screen, so this is
// the natural mobile home for theme + accent + density preferences
// (the prototype's floating Tweaks panel collapses into this tab here).

import { useTheme, type ThemePreference } from "@/lib/theme-provider";

import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, type MobilePalette, usePalette } from "./palette";
import { SectionLabel } from "./primitives";

const ACCENTS = ["#2f9e6a", "#3b6fe0", "#7a4dd1", "#d97757", "#1f8a8a"] as const;

export function MobileMe({
  dark,
  accent,
  setAccent,
  density,
  setDensity,
}: {
  dark: boolean;
  accent: string;
  setAccent: (v: string) => void;
  density: "compact" | "regular";
  setDensity: (v: "compact" | "regular") => void;
}) {
  const p = usePalette(dark);
  const { theme, setTheme } = useTheme();

  return (
    <div
      style={{
        height: "100%",
        overflow: "auto",
        background: p.bg,
        paddingBottom: 96,
        fontFamily: MOBILE_FONT_SANS,
      }}
    >
      <div style={{ padding: "max(env(safe-area-inset-top), 44px) 20px 8px" }}>
        <h1
          style={{
            margin: 0,
            fontSize: 32,
            fontWeight: 700,
            color: p.text,
            letterSpacing: "-0.025em",
          }}
        >
          Me
        </h1>
        <p style={{ margin: "4px 0 0", fontSize: 13.5, color: p.text2 }}>
          Theme, accent, and layout density.
        </p>
      </div>

      <SectionLabel dark={dark}>Theme</SectionLabel>
      <div style={{ padding: "0 14px" }}>
        <Card palette={p}>
          <SegmentedRow
            options={[
              { id: "system", label: "System" },
              { id: "light", label: "Light" },
              { id: "dark", label: "Dark" },
            ]}
            value={theme}
            onChange={(v) => setTheme(v as ThemePreference)}
            palette={p}
            dark={dark}
          />
        </Card>
      </div>

      <SectionLabel dark={dark}>Accent</SectionLabel>
      <div style={{ padding: "0 14px" }}>
        <Card palette={p}>
          <div style={{ display: "flex", gap: 12, padding: "12px 4px", flexWrap: "wrap" }}>
            {ACCENTS.map((c) => {
              const on = c === accent;
              return (
                <button
                  key={c}
                  type="button"
                  onClick={() => setAccent(c)}
                  aria-label={`Set accent ${c}`}
                  style={{
                    width: 36,
                    height: 36,
                    borderRadius: 999,
                    cursor: "pointer",
                    background: c,
                    border: on ? `2px solid ${p.text}` : "2px solid transparent",
                    boxShadow: on ? `0 0 0 2px ${p.bg} inset` : "none",
                  }}
                />
              );
            })}
          </div>
        </Card>
      </div>

      <SectionLabel dark={dark}>Density</SectionLabel>
      <div style={{ padding: "0 14px" }}>
        <Card palette={p}>
          <SegmentedRow
            options={[
              { id: "regular", label: "Regular" },
              { id: "compact", label: "Compact" },
            ]}
            value={density}
            onChange={(v) => setDensity(v as "regular" | "compact")}
            palette={p}
            dark={dark}
          />
        </Card>
      </div>

      <div
        style={{
          padding: "24px 20px",
          fontFamily: MOBILE_FONT_MONO,
          fontSize: 11,
          color: p.text3,
          letterSpacing: "0.04em",
        }}
      >
        Mobile design preview · v0.1
      </div>
    </div>
  );
}

function Card({
  palette,
  children,
}: {
  palette: MobilePalette;
  children: React.ReactNode;
}) {
  return (
    <div
      style={{
        background: palette.surface,
        borderRadius: 16,
        border: `0.5px solid ${palette.border}`,
        padding: "4px 14px",
      }}
    >
      {children}
    </div>
  );
}

function SegmentedRow({
  options,
  value,
  onChange,
  palette,
  dark,
}: {
  options: { id: string; label: string }[];
  value: string;
  onChange: (v: string) => void;
  palette: MobilePalette;
  dark: boolean;
}) {
  return (
    <div style={{ display: "flex", gap: 6, padding: "10px 0" }}>
      {options.map((o) => {
        const on = o.id === value;
        return (
          <button
            key={o.id}
            type="button"
            onClick={() => onChange(o.id)}
            style={{
              flex: 1,
              padding: "10px 8px",
              borderRadius: 10,
              cursor: "pointer",
              background: on ? palette.text : "transparent",
              color: on ? (dark ? palette.bg : "#fff") : palette.text,
              border: `1px solid ${on ? "transparent" : palette.border}`,
              fontSize: 13,
              fontWeight: 600,
            }}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}
