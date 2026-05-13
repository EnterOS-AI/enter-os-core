"use client";

// MobileApp — top-level mobile shell.
// Local route state, bottom tab bar, theme-aware palette. Only rendered
// on viewports < 640px (see app/page.tsx). The desktop Canvas is not
// instantiated when MobileApp is active, so no React Flow + heavy
// chrome cost on phones.

import { useEffect, useMemo, useState } from "react";

import { useTheme } from "@/lib/theme-provider";

import { TabBar, type MobileTabId } from "./components";
import { MobileCanvas } from "./MobileCanvas";
import { MobileChat } from "./MobileChat";
import { MobileComms } from "./MobileComms";
import { MobileDetail } from "./MobileDetail";
import { MobileHome } from "./MobileHome";
import { MobileMe } from "./MobileMe";
import { MobileSpawn } from "./MobileSpawn";
import { usePalette } from "./palette";
import { MobileAccentProvider } from "./palette-context";
import { SearchDialog } from "@/components/SearchDialog";

type Route = "home" | "canvas" | "detail" | "chat" | "comms" | "me";

const ROUTES: Route[] = ["home", "canvas", "detail", "chat", "comms", "me"];

const ACCENT_KEY = "molecule.mobile.accent";
const DENSITY_KEY = "molecule.mobile.density";

function readStored<T extends string>(key: string, fallback: T, allowed?: T[]): T {
  if (typeof window === "undefined") return fallback;
  try {
    const v = window.localStorage.getItem(key);
    if (!v) return fallback;
    if (allowed && !allowed.includes(v as T)) return fallback;
    return v as T;
  } catch {
    return fallback;
  }
}

interface UrlState {
  route: Route;
  agentId: string | null;
}

/** Parse the current URL into a (route, agentId) pair. Reads from
 *  `?m=<route>&a=<agentId>` — `home` is the default when `m` is
 *  absent. Detail/chat without an agent id collapse back to `home`
 *  because they're meaningless without one. */
function readRouteFromUrl(): UrlState {
  if (typeof window === "undefined") return { route: "home", agentId: null };
  const params = new URLSearchParams(window.location.search);
  const m = params.get("m");
  const a = params.get("a");
  const route: Route = ROUTES.includes(m as Route) ? (m as Route) : "home";
  if ((route === "detail" || route === "chat") && !a) {
    return { route: "home", agentId: null };
  }
  return { route, agentId: a };
}

/** Build the canonical URL for a (route, agentId) pair, preserving any
 *  unrelated search params and the existing hash. `home` is the default
 *  state, so we drop `m` from the URL to keep the no-state link clean. */
function buildRouteUrl(route: Route, agentId: string | null): string {
  if (typeof window === "undefined") return "";
  const params = new URLSearchParams(window.location.search);
  if (route === "home") params.delete("m");
  else params.set("m", route);
  if (agentId && (route === "detail" || route === "chat")) params.set("a", agentId);
  else params.delete("a");
  const search = params.toString();
  return window.location.pathname + (search ? "?" + search : "") + window.location.hash;
}

export function MobileApp() {
  const { resolvedTheme } = useTheme();
  const dark = resolvedTheme === "dark";
  const p = usePalette(dark);

  // Seed route + agentId from the URL so deep links like
  // `/?m=detail&a=ws-42` open straight on the right screen.
  const [route, setRoute] = useState<Route>(() => readRouteFromUrl().route);
  const [agentId, setAgentId] = useState<string | null>(() => readRouteFromUrl().agentId);
  const [showSpawn, setShowSpawn] = useState(false);

  // Sync route state → URL via history.pushState. Skip the push when
  // the URL is already what we'd produce — that handles the initial
  // mount (we read FROM the URL) and prevents duplicate history entries
  // when popstate restores state we just pushed.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const current = readRouteFromUrl();
    if (current.route === route && current.agentId === agentId) return;
    const url = buildRouteUrl(route, agentId);
    window.history.pushState({ route, agentId }, "", url);
  }, [route, agentId]);

  // Sync URL → route state on browser back/forward. The popstate event
  // fires AFTER the URL has changed, so re-reading is correct.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const onPop = () => {
      const next = readRouteFromUrl();
      setRoute(next.route);
      setAgentId(next.agentId);
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  const [accent, setAccentState] = useState<string>(() => readStored(ACCENT_KEY, "#2f9e6a"));
  const [density, setDensityState] = useState<"compact" | "regular">(() =>
    readStored<"compact" | "regular">(DENSITY_KEY, "regular", ["compact", "regular"]),
  );

  // Persist accent. The accent itself is propagated into every palette
  // read via React context (MobileAccentProvider below) — never by
  // mutating the MOL_LIGHT/MOL_DARK singletons.
  useEffect(() => {
    try {
      window.localStorage.setItem(ACCENT_KEY, accent);
    } catch {
      /* noop */
    }
  }, [accent]);
  useEffect(() => {
    try {
      window.localStorage.setItem(DENSITY_KEY, density);
    } catch {
      /* noop */
    }
  }, [density]);

  const activeTab: MobileTabId = useMemo(() => {
    if (route === "canvas") return "canvas";
    if (route === "comms") return "comms";
    if (route === "me") return "me";
    return "agents";
  }, [route]);

  const onTabChange = (id: MobileTabId) => {
    if (id === "agents") setRoute("home");
    else if (id === "canvas") setRoute("canvas");
    else if (id === "comms") setRoute("comms");
    else if (id === "me") setRoute("me");
  };

  const openAgent = (id: string) => {
    setAgentId(id);
    setRoute("detail");
  };

  // Tab bar visible everywhere except chat (per design).
  const showTabBar = route !== "chat";

  return (
    <MobileAccentProvider accent={accent}>
    <main
      style={{
        position: "fixed",
        inset: 0,
        background: p.bg,
        color: p.text,
        overflow: "hidden",
        contain: "strict",
      }}
    >
      {route === "home" && (
        <MobileHome
          dark={dark}
          density={density}
          onOpen={openAgent}
          onSpawn={() => setShowSpawn(true)}
        />
      )}
      {route === "canvas" && (
        <MobileCanvas dark={dark} onOpen={openAgent} onSpawn={() => setShowSpawn(true)} />
      )}
      {route === "detail" && agentId && (
        <MobileDetail
          agentId={agentId}
          dark={dark}
          onBack={() => setRoute("home")}
          onChat={() => setRoute("chat")}
        />
      )}
      {route === "chat" && agentId && (
        <MobileChat agentId={agentId} dark={dark} onBack={() => setRoute("detail")} />
      )}
      {route === "comms" && <MobileComms dark={dark} />}
      {route === "me" && (
        <MobileMe
          dark={dark}
          accent={accent}
          setAccent={setAccentState}
          density={density}
          setDensity={setDensityState}
        />
      )}

      {showTabBar && <TabBar dark={dark} active={activeTab} onChange={onTabChange} />}

      {showSpawn && <MobileSpawn dark={dark} onClose={() => setShowSpawn(false)} />}

      <SearchDialog />
    </main>
    </MobileAccentProvider>
  );
}
