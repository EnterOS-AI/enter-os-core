"use client";

// 06 · Spawn agent — bottom-sheet flow.
// Fetches /templates so the user picks from what's actually installed
// on this platform (no hardcoded ID guesswork). Posts to /workspaces
// with the same shape useTemplateDeploy uses. Skips the secret-key
// preflight — if a deploy needs missing keys, the API surfaces the
// error and we show it with a hint to fall through to the desktop
// dialog (which has the full preflight + key-import flow).

import { useEffect, useState } from "react";

import { api } from "@/lib/api";
import { type Template } from "@/lib/deploy-preflight";
import { isSaaSTenant } from "@/lib/tenant";

import { tierCode } from "./palette";
import { MOBILE_FONT_MONO, MOBILE_FONT_SANS, type MobilePalette, usePalette } from "./palette";
import { Icons, SectionLabel, TierChip } from "./primitives";

const TIER_LABEL: Record<"T1" | "T2" | "T3" | "T4", string> = {
  T1: "Sandboxed",
  T2: "Standard",
  T3: "Privileged",
  T4: "Full Access",
};

export function MobileSpawn({ dark, onClose }: { dark: boolean; onClose: () => void }) {
  const p = usePalette(dark);
  const isSaaS = isSaaSTenant();
  const [templates, setTemplates] = useState<Template[]>([]);
  const [loadingTemplates, setLoadingTemplates] = useState(true);
  const [tplId, setTplId] = useState<string | null>(null);
  const [tier, setTier] = useState<"T1" | "T2" | "T3" | "T4">("T2");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .get<Template[]>("/templates")
      .then((list) => {
        if (cancelled) return;
        setTemplates(list);
        if (list.length > 0) {
          setTplId(list[0].id);
          setTier(isSaaS ? "T4" : tierCode(list[0].tier));
        }
      })
      .catch(() => {
        if (!cancelled) setTemplates([]);
      })
      .finally(() => {
        if (!cancelled) setLoadingTemplates(false);
      });
    return () => {
      cancelled = true;
    };
  }, [isSaaS]);

  const handleSpawn = async () => {
    if (busy || !tplId) return;
    const chosen = templates.find((t) => t.id === tplId);
    if (!chosen) return;
    setError(null);
    setBusy(true);
    try {
      await api.post<{ id: string }>("/workspaces", {
        name: (name.trim() || chosen.name),
        template: chosen.id,
        tier: isSaaS ? 4 : Number(tier.slice(1)),
        canvas: {
          x: Math.random() * 400 + 100,
          y: Math.random() * 300 + 100,
        },
      });
      onClose();
    } catch (e) {
      setError(
        e instanceof Error
          ? `${e.message}. If this template needs missing API keys, use the desktop palette to import them.`
          : "Spawn failed",
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Spawn agent"
      style={{
        position: "absolute",
        inset: 0,
        zIndex: 100,
        background: "rgba(20,15,10,0.42)",
        backdropFilter: "blur(4px)",
        display: "flex",
        alignItems: "flex-end",
        fontFamily: MOBILE_FONT_SANS,
      }}
      onClick={(e) => {
        // Click on the dim backdrop closes the sheet.
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        style={{
          width: "100%",
          background: p.bg,
          borderRadius: "24px 24px 0 0",
          maxHeight: "88%",
          overflow: "auto",
          boxShadow: "0 -10px 40px rgba(0,0,0,0.18)",
        }}
      >
        <Grabber palette={p} />

        {/* Header */}
        <div
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            padding: "6px 18px 10px",
          }}
        >
          <div>
            <h2
              style={{
                margin: 0,
                fontSize: 22,
                fontWeight: 700,
                color: p.text,
                letterSpacing: "-0.02em",
              }}
            >
              Spawn Agent
            </h2>
            <p style={{ margin: "2px 0 0", fontSize: 12.5, color: p.text2 }}>
              In workspace · Default
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            style={{
              width: 32,
              height: 32,
              borderRadius: 999,
              cursor: "pointer",
              background: dark ? "#22211c" : "#fff",
              border: `0.5px solid ${p.border}`,
              color: p.text2,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            {Icons.close({ size: 16 })}
          </button>
        </div>

        {/* Templates */}
        <SectionLabel dark={dark}>Template</SectionLabel>
        <div style={{ padding: "0 14px" }}>
          {loadingTemplates ? (
            <div
              style={{
                padding: "24px 8px",
                textAlign: "center",
                color: p.text3,
                fontSize: 13,
              }}
            >
              Loading templates…
            </div>
          ) : templates.length === 0 ? (
            <div
              style={{
                padding: "16px 14px",
                background: p.surface,
                borderRadius: 14,
                border: `0.5px solid ${p.border}`,
                color: p.text2,
                fontSize: 13,
                lineHeight: 1.45,
              }}
            >
              No templates installed on this platform yet. Open the desktop canvas
              and use the template palette to import one (Claude Code, Hermes, or
              an org template), then come back here to spawn.
            </div>
          ) : (
            <div
              style={{
                display: "grid",
                gridTemplateColumns: "1fr 1fr",
                gap: 8,
              }}
            >
              {templates.map((t) => {
                const on = tplId === t.id;
                const tCode = isSaaS ? "T4" : tierCode(t.tier);
                return (
                  <button
                    key={t.id}
                    type="button"
                    onClick={() => {
                      setTplId(t.id);
                      setTier(tCode);
                    }}
                    style={{
                      background: on
                        ? dark
                          ? "#2a2823"
                          : "#fff"
                        : dark
                          ? "#1d1c17"
                          : "#fbf9f4",
                      border: `1px solid ${on ? p.accent : p.border}`,
                      borderRadius: 14,
                      padding: "12px 12px",
                      textAlign: "left",
                      cursor: "pointer",
                      display: "flex",
                      flexDirection: "column",
                      gap: 4,
                      position: "relative",
                    }}
                  >
                    <div
                      style={{
                        display: "flex",
                        alignItems: "center",
                        justifyContent: "space-between",
                        gap: 6,
                      }}
                    >
                      <span
                        style={{
                          fontSize: 13.5,
                          fontWeight: 600,
                          color: p.text,
                          overflow: "hidden",
                          textOverflow: "ellipsis",
                          whiteSpace: "nowrap",
                        }}
                      >
                        {t.name}
                      </span>
                      <TierChip tier={tCode} dark={dark} />
                    </div>
                    {t.description && (
                      <span
                        style={{
                          fontSize: 11.5,
                          color: p.text2,
                          lineHeight: 1.35,
                          display: "-webkit-box",
                          WebkitLineClamp: 2,
                          WebkitBoxOrient: "vertical",
                          overflow: "hidden",
                        }}
                      >
                        {t.description}
                      </span>
                    )}
                    {on && (
                      <span
                        style={{
                          position: "absolute",
                          top: 8,
                          right: 8,
                          width: 16,
                          height: 16,
                          borderRadius: 999,
                          background: p.accent,
                          color: "#fff",
                          display: "flex",
                          alignItems: "center",
                          justifyContent: "center",
                        }}
                      >
                        {Icons.check({ size: 10, sw: 2.5 })}
                      </span>
                    )}
                  </button>
                );
              })}
            </div>
          )}
        </div>

        {/* Name */}
        <SectionLabel dark={dark}>Name</SectionLabel>
        <div style={{ padding: "0 14px" }}>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={tplId
              ? (templates.find((t) => t.id === tplId)?.name ?? "agent-name")
              : "agent-name"}
            style={{
              width: "100%",
              padding: "12px 14px",
              background: dark ? "#22211c" : "#fff",
              border: `0.5px solid ${p.border}`,
              borderRadius: 12,
              fontFamily: MOBILE_FONT_MONO,
              fontSize: 13.5,
              color: p.text,
              outline: "none",
              boxSizing: "border-box",
            }}
          />
        </div>

        {/* Tier */}
        <SectionLabel dark={dark}>Permission tier</SectionLabel>
        <div style={{ padding: "0 14px", display: "flex", gap: 6 }}>
          {(["T1", "T2", "T3", "T4"] as const).map((t) => {
            const on = tier === t;
            return (
              <button
                key={t}
                type="button"
                onClick={() => setTier(t)}
                style={{
                  flex: 1,
                  padding: "10px 8px",
                  cursor: "pointer",
                  background: on ? (dark ? "#22211c" : "#fff") : "transparent",
                  border: `1px solid ${on ? p.accent : p.border}`,
                  borderRadius: 12,
                  display: "flex",
                  flexDirection: "column",
                  alignItems: "center",
                  gap: 4,
                }}
              >
                <TierChip tier={t} dark={dark} size="lg" />
                <span style={{ fontSize: 10.5, color: p.text2, fontWeight: 500 }}>
                  {TIER_LABEL[t]}
                </span>
              </button>
            );
          })}
        </div>

        {/* Error */}
        {error && (
          <div
            role="alert"
            style={{
              margin: "12px 14px 0",
              padding: "10px 14px",
              background: `${p.failed}1a`,
              border: `0.5px solid ${p.failed}40`,
              borderRadius: 12,
              color: p.failed,
              fontSize: 12.5,
              lineHeight: 1.4,
            }}
          >
            {error}
          </div>
        )}

        {/* Spawn button */}
        <div style={{ padding: "20px 14px max(env(safe-area-inset-bottom), 28px)" }}>
          <button
            type="button"
            onClick={handleSpawn}
            disabled={busy || !tplId || templates.length === 0}
            style={{
              width: "100%",
              height: 52,
              borderRadius: 16,
              border: "none",
              cursor: busy ? "wait" : tplId ? "pointer" : "not-allowed",
              background: p.text,
              color: dark ? p.bg : "#fff",
              fontSize: 15,
              fontWeight: 600,
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
              gap: 10,
              boxShadow: "0 8px 22px rgba(40,30,20,0.22)",
              opacity: busy || !tplId ? 0.55 : 1,
            }}
          >
            {Icons.zap({ size: 16 })} {busy ? "Spawning…" : "Spawn agent"}
          </button>
          <p
            style={{
              margin: "10px 0 0",
              textAlign: "center",
              fontSize: 11.5,
              color: p.text3,
              lineHeight: 1.4,
            }}
          >
            Boots in ~3s. Tier {tier} permissions apply on first call.
          </p>
        </div>
      </div>
    </div>
  );
}

function Grabber({ palette }: { palette: MobilePalette }) {
  return (
    <div style={{ display: "flex", justifyContent: "center", padding: "8px 0 4px" }}>
      <span
        style={{
          width: 38,
          height: 4,
          borderRadius: 999,
          background: palette.text3,
          opacity: 0.4,
        }}
      />
    </div>
  );
}
