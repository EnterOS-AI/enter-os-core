"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useCanvasStore, type TopView } from "@/store/canvas";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";
import { useTheme } from "@/lib/theme-provider";
import { api, PLATFORM_URL } from "@/lib/api";
import { switchOrgUrl } from "@/lib/org-switch";
import type { ActivityEntry } from "@/types/activity";
import { Canvas } from "@/components/Canvas";
import { CommunicationOverlay } from "@/components/CommunicationOverlay";
import { MessageFlightHome } from "./MessageFlightHome";
import { ChatTab } from "@/components/tabs/ChatTab";
import { WorkspacePanelTabs } from "@/components/WorkspacePanelTabs";
import { SettingsTabs } from "@/components/settings";
import s from "./Concierge.module.css";
import { RequestsInbox } from "./RequestsInbox";
import {
  IcHome, IcOrgMap, IcSettings, IcSearch, IcBell, IcSun, IcMoon, IcChevDown,
  IcQueue, IcCaret, IcMolecule, IcCheck, IcChat,
} from "./icons";

/* ── status → concept palette ─────────────────────────────────────────── */
function statusInfo(status: string): { color: string; label: string } {
  switch (status) {
    case "online": return { color: "var(--green)", label: "online" };
    case "provisioning":
    case "starting": return { color: "var(--amber)", label: "starting" };
    case "degraded": return { color: "var(--amber)", label: "degraded" };
    case "building": return { color: "var(--amber)", label: "building" };
    case "failed": return { color: "var(--red)", label: "failed" };
    case "paused": return { color: "var(--accent-2)", label: "paused" };
    default: return { color: "var(--grey)", label: status || "idle" };
  }
}

const AV_GRADIENTS = [
  "linear-gradient(150deg,#a78bfa,#7c3aed)",
  "linear-gradient(150deg,#60a5fa,#3b82f6)",
  "linear-gradient(150deg,#34d399,#10b981)",
  "linear-gradient(150deg,#fbbf77,#f59e0b)",
  "linear-gradient(150deg,#5eead4,#14b8a6)",
  "linear-gradient(150deg,#f0a36b,#e8638a)",
];
function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}
function gradientFor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return AV_GRADIENTS[h % AV_GRADIENTS.length];
}

type SbTab = "agents" | "tasks" | "approvals";

/** ISO timestamp → "9:05 PM" (local). Empty string on a bad/missing value. */
function clockTime(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}

/** A human action label from an activity row. */
function activityText(a: ActivityEntry): string {
  if (a.summary) return a.summary;
  const verb = a.activity_type?.replace(/_/g, " ") ?? "activity";
  return a.method ? `${verb} · ${a.method}` : verb;
}


/**
 * resolveHomeChatTarget — which agent the Home chat panel talks to: the
 * sidebar-selected node when it still exists, else the org root (concierge),
 * else null. Resolving against live nodes means a deleted/vanished selection
 * degrades to the root instead of a dead chat. Exported for unit tests.
 */
export function resolveHomeChatTarget<N extends { id: string }>(
  nodes: N[],
  selectedNodeId: string | null,
  platformRoot: N | null,
): N | null {
  if (selectedNodeId) {
    const selected = nodes.find((n) => n.id === selectedNodeId);
    if (selected) return selected;
  }
  return platformRoot ?? null;
}

export function ConciergeShell() {
  const nodes = useCanvasStore((st) => st.nodes);
  const topView = useCanvasStore((st) => st.topView);
  const setTopView = useCanvasStore((st) => st.setTopView);
  const selectNode = useCanvasStore((st) => st.selectNode);
  const selectedNodeId = useCanvasStore((st) => st.selectedNodeId);
  const { resolvedTheme, setTheme } = useTheme();

  const [railOpen, setRailOpen] = useState(false);
  const [sbTab, setSbTab] = useState<SbTab>("agents");
  const [settingsTab, setSettingsTab] = useState<"platform" | "org">("platform");
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});

  // Dynamic org name for the topbar. Sourced from GET /org/identity
  // ({name} ← MOLECULE_ORG_NAME, added by a parallel backend change).
  // Falls back to "Molecule AI" when the endpoint 404s / errors or
  // returns an empty name, so the topbar never breaks before the backend
  // lands.
  const [orgName, setOrgName] = useState("Molecule AI");
  // Current org slug (from GET /org/identity) — used to highlight the active
  // org in the switcher and to derive the apex domain for cross-org navigation.
  const [orgSlug, setOrgSlug] = useState("");
  useEffect(() => {
    let cancelled = false;
    api
      .get<{ name?: string; slug?: string }>("/org/identity")
      .then((r) => {
        const name = (r?.name || "").trim();
        if (!cancelled && name) setOrgName(name);
        const slug = (r?.slug || "").trim();
        if (!cancelled && slug) setOrgSlug(slug);
      })
      .catch(() => {
        // No endpoint / not reachable — keep the "Molecule AI" fallback.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // --- Org switcher (topbar dropdown) ---
  // Each org is its own tenant subdomain, so "switch" = navigate to
  // <slug>.<apex>. The org list comes from the control plane (cross-origin,
  // cookie-auth), fetched lazily the first time the menu opens.
  const [orgMenuOpen, setOrgMenuOpen] = useState(false);
  const [orgs, setOrgs] = useState<Array<{ slug: string; name?: string; id?: string }> | null>(null);
  const toggleOrgMenu = useCallback(() => {
    setOrgMenuOpen((open) => {
      const next = !open;
      // Reset orgs to null when reopening after a previous error so we
      // don't cache the empty "No other organizations" state forever.
      // (core#2509)
      if (next && orgs !== null && orgs.length === 0) {
        setOrgs(null);
      }
      return next;
    });
  }, [orgs]);
  // Fetch orgs when the menu opens and we have no cached list.
  // Kept outside the setState updater to avoid StrictMode double-fetch.
  // (core#2509)
  useEffect(() => {
    if (!orgMenuOpen || orgs !== null) return;
    fetch(`${PLATFORM_URL}/cp/orgs`, {
      credentials: "include",
      signal: AbortSignal.timeout(15_000),
    })
      .then((res) => (res.ok ? res.json() : Promise.reject(new Error(String(res.status)))))
      .then((body: { orgs?: Array<{ slug: string; name?: string; id?: string }> } | Array<{ slug: string; name?: string; id?: string }>) => {
        const list = Array.isArray(body) ? body : body.orgs ?? [];
        setOrgs(list.filter((o) => o && o.slug));
      })
      .catch(() => setOrgs([])); // no list / not reachable → render "no other orgs"
  }, [orgMenuOpen, orgs]);
  const switchOrg = useCallback(
    (slug: string) => {
      setOrgMenuOpen(false);
      if (typeof window === "undefined") return;
      const url = switchOrgUrl(window.location.host, window.location.protocol, orgSlug, slug);
      if (url) window.location.href = url;
    },
    [orgSlug]
  );
  // Close the menu on any outside click.
  useEffect(() => {
    if (!orgMenuOpen) return;
    const onDoc = () => setOrgMenuOpen(false);
    document.addEventListener("click", onDoc);
    return () => document.removeEventListener("click", onDoc);
  }, [orgMenuOpen]);

  // Build the agent hierarchy from live nodes.
  const { roots, childrenOf } = useMemo(() => {
    const childrenOf = new Map<string, typeof nodes>();
    const roots: typeof nodes = [];
    for (const n of nodes) {
      const p = n.data.parentId;
      if (p) {
        const arr = childrenOf.get(p) ?? [];
        arr.push(n);
        childrenOf.set(p, arr);
      } else {
        roots.push(n);
      }
    }
    return { roots, childrenOf };
  }, [nodes]);

  const platformRoot = useMemo(
    () =>
      // Resolve the platform agent by the authoritative kind='platform' marker
      // only — the backend in this branch always returns kind
      // (COALESCE(w.kind,'workspace')) and the map-side filter
      // (canvas-topology/Canvas/Toolbar) is kind-only, so the shell must not
      // disagree via a name/role heuristic. Fall back to the first root only as
      // graceful degradation if no node is tagged platform.
      roots.find((r) => r.data.kind === WORKSPACE_KIND.Platform) ??
      roots[0] ??
      null,
    [roots],
  );

  const platformId = platformRoot?.id ?? null;

  // Home chat target: the agent SELECTED in the sidebar, falling back to the
  // org root (the concierge). Pre-fix the panel was hard-pointed at the root,
  // so clicking another agent highlighted it but the chat never switched.
  const chatNode = useMemo(
    () => resolveHomeChatTarget(nodes, selectedNodeId, platformRoot),
    [nodes, selectedNodeId, platformRoot],
  );
  const chatId = chatNode?.id ?? null;
  const chatIsRoot = chatId !== null && chatId === platformId;

  // ── live data: requests (Tasks + Approvals, org-wide), activity (platform agent) ──
  // The Tasks/Approvals tabs are now driven by the unified RequestsInbox
  // component (RFC unified-requests-inbox P3) over /requests/pending?kind=…;
  // the shell keeps only the per-tab pending counts so the tab badges render.
  // The inbox owns its own fetch, optimistic update, live-refresh and
  // More-Info thread — see RequestsInbox.tsx.
  const [taskCount, setTaskCount] = useState(0);
  const [apprCount, setApprCount] = useState(0);
  const [activity, setActivity] = useState<ActivityEntry[]>([]);

  useEffect(() => {
    if (!platformId) return;
    let cancelled = false;
    api.get<ActivityEntry[]>(`/workspaces/${platformId}/activity?limit=12`)
      .then((r) => { if (!cancelled) setActivity(r ?? []); })
      .catch(() => { if (!cancelled) setActivity([]); });
    return () => { cancelled = true; };
  }, [platformId]);

  const nav = (v: TopView) => setTopView(v);

  /* ── agents tree (recursive) ──────────────────────────────────────── */
  function renderNode(n: (typeof nodes)[number], depth: number) {
    const kids = childrenOf.get(n.id) ?? [];
    const hasKids = kids.length > 0;
    const isCollapsed = collapsed[n.id];
    const st = statusInfo(n.data.status);
    const isRoot = depth === 0;
    const isPlatform = n.id === platformRoot?.id;
    const q = (n.data.activeTasks as number) ?? 0;
    // Role can be a long descriptor (e.g. "Coding Executor (Kimi) — …"); render
    // it compact (single-line, truncated by .wsRole) and surface the full text
    // on hover via the native tooltip.
    const roleLabel = isPlatform ? "platform" : n.data.role || "agent";
    const row = (
      <div
        role="button"
        tabIndex={0}
        data-testid="agent-tree-node"
        data-node-name={n.data.name}
        data-ws-id={n.id}
        data-platform={isPlatform ? "true" : "false"}
        data-depth={depth}
        className={`${s.ws} ${selectedNodeId === n.id ? s.active : ""}`}
        onClick={() => selectNode(n.id)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            selectNode(n.id);
          }
        }}
      >
        <div className={s.wsAv} style={{ background: gradientFor(n.id) }}>
          {initials(n.data.name)}
          <span className={s.dot} style={{ background: st.color }} />
        </div>
        <div className={s.wsMeta}>
          <div className={s.wsName}>{n.data.name}</div>
          <div className={s.wsSub}>
            <span className={s.wsRole} title={roleLabel}>{roleLabel}</span>
            <span className={s.wsStatus} style={{ color: st.color }}>
              <span className={s.sdot} style={{ background: st.color }} />
              {st.label}
            </span>
          </div>
        </div>
        {isRoot && isPlatform ? (
          <span data-testid="agent-tree-root-tag" className={s.rootTag}>root</span>
        ) : (
          <span className={`${s.wsQ} ${q === 0 ? s.zero : ""}`} title="Tasks in queue">
            <IcQueue />
            {q}
          </span>
        )}
        {hasKids && (
          <button
            className={s.wsCaret}
            title="Expand / collapse"
            onClick={(e) => {
              e.stopPropagation();
              setCollapsed((c) => ({ ...c, [n.id]: !c[n.id] }));
            }}
            style={{ transform: isCollapsed ? "none" : "rotate(90deg)", transition: "transform .18s" }}
          >
            <IcCaret />
          </button>
        )}
      </div>
    );
    return (
      <div key={n.id} className={s.tnode}>
        {row}
        {hasKids && !isCollapsed && (
          <div className={s.treeChildren}>
            {kids.map((k) => renderNode(k, depth + 1))}
          </div>
        )}
      </div>
    );
  }

  return (
    <div className={s.root}>
      {/* Envelope flies between agent rows on each delegate/message event. */}
      <MessageFlightHome />
      <div className={`${s.app} ${railOpen ? s.railOpen : ""}`}>
        {/* ICON RAIL */}
        <nav className={s.rail}>
          <div className={s.railTop}>
            <div className={s.logo} title="Toggle sidebar" onClick={() => setRailOpen((o) => !o)}>
              <IcMolecule />
            </div>
            <span className={s.railWordmark}>Molecule</span>
            <button className={s.railToggle} title="Collapse sidebar" onClick={() => setRailOpen((o) => !o)}>
              <IcOrgMap />
            </button>
          </div>
          <button data-testid="nav-home" className={`${s.navbtn} ${topView === "home" ? s.active : ""}`} title="Home" onClick={() => nav("home")}>
            <span className={s.ico}><IcHome /></span><span className={s.lbl}>Home</span>
          </button>
          <button data-testid="nav-map" className={`${s.navbtn} ${topView === "map" ? s.active : ""}`} title="Org map" onClick={() => nav("map")}>
            <span className={s.ico}><IcOrgMap /></span><span className={s.lbl}>Org map</span>
          </button>
          <div className={s.spacer} />
          <button data-testid="nav-settings" className={`${s.navbtn} ${topView === "settings" ? s.active : ""}`} title="Settings" onClick={() => nav("settings")}>
            <span className={s.ico}><IcSettings /></span><span className={s.lbl}>Settings</span>
          </button>
        </nav>

        <div className={s.main}>
          {/* TOPBAR */}
          <header className={s.topbar}>
            <div
              className={s.org}
              role="button"
              tabIndex={0}
              aria-haspopup="menu"
              aria-expanded={orgMenuOpen}
              data-testid="topbar-org-switcher"
              onClick={(e) => {
                e.stopPropagation();
                toggleOrgMenu();
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  e.stopPropagation();
                  toggleOrgMenu();
                }
              }}
            >
              <div className={s.orgBadge}>{initials(orgName).slice(0, 1)}</div>
              <span data-testid="topbar-org-name" className={s.orgName}>{orgName}</span>
              <span className={s.chev}><IcChevDown /></span>
              {orgMenuOpen && (
                <div
                  className={s.orgMenu}
                  role="menu"
                  data-testid="topbar-org-menu"
                  onClick={(e) => e.stopPropagation()}
                >
                  {orgs === null ? (
                    <div className={s.orgMenuEmpty}>Loading…</div>
                  ) : orgs.length === 0 ? (
                    <div className={s.orgMenuEmpty}>No other organizations</div>
                  ) : (
                    orgs.map((o) => (
                      <button
                        key={o.id || o.slug}
                        type="button"
                        role="menuitem"
                        className={`${s.orgMenuItem} ${o.slug === orgSlug ? s.orgMenuCurrent : ""}`}
                        onClick={() => switchOrg(o.slug)}
                      >
                        <span className={s.orgMenuBadge}>{initials(o.name || o.slug).slice(0, 1)}</span>
                        <span className={s.orgMenuName}>{o.name || o.slug}</span>
                        {o.slug === orgSlug && (
                          <span className={s.orgMenuTick}><IcCheck /></span>
                        )}
                      </button>
                    ))
                  )}
                </div>
              )}
            </div>
            <div className={s.topbarRight}>
              <button className={s.iconPill} title="Search"><IcSearch /></button>
              <button className={s.iconPill} title="Notifications"><IcBell /></button>
              <button
                className={s.themeToggle}
                title="Toggle theme"
                onClick={() => setTheme(resolvedTheme === "dark" ? "light" : "dark")}
              >
                {resolvedTheme === "dark" ? <IcMoon /> : <IcSun />}
              </button>
              <div className={s.avatar} title="You">HW</div>
            </div>
          </header>

          <div className={s.viewArea}>
            {/* HOME VIEW */}
            <div className={`${s.view} ${topView === "home" ? s.active : ""}`}>
              <aside className={s.homeSidebar}>
                <div className={s.sbTabs}>
                  <button data-testid="home-subtab-agents" className={`${s.sbTab} ${sbTab === "agents" ? s.active : ""}`} onClick={() => setSbTab("agents")}>Agents</button>
                  <button data-testid="home-subtab-tasks" className={`${s.sbTab} ${sbTab === "tasks" ? s.active : ""}`} onClick={() => setSbTab("tasks")}>
                    Tasks{taskCount > 0 && <span className={s.cnt}>{taskCount}</span>}
                  </button>
                  <button data-testid="home-subtab-approvals" className={`${s.sbTab} ${sbTab === "approvals" ? s.active : ""}`} onClick={() => setSbTab("approvals")}>
                    Approvals{apprCount > 0 && <span className={s.cnt}>{apprCount}</span>}
                  </button>
                </div>
                <div className={s.sbBody}>
                  {sbTab === "agents" && (
                    <>
                      <div className={s.wsList}>
                        {roots.length === 0 && (
                          <div className={s.empty}>No agents yet. Ask the concierge to spin up a team.</div>
                        )}
                        {roots.map((r) => renderNode(r, 0))}
                      </div>
                      <div className={s.sbSection}>Recent activity</div>
                      <div>
                        {activity.length === 0 && (
                          <div className={s.empty}>No recent activity yet.</div>
                        )}
                        {activity.map((a) => {
                          const ok = a.status !== "error" && a.status !== "failed";
                          return (
                            <div key={a.id} className={s.act}>
                              <span className={s.actTime}>{clockTime(a.created_at)}</span>
                              <div className={`${s.actLine} ${ok ? s.grn : ""}`}>
                                <div className={s.actText}>{activityText(a)}</div>
                              </div>
                            </div>
                          );
                        })}
                      </div>
                    </>
                  )}
                  {/* Both inboxes stay mounted so their pending-count badges
                      remain live on the tab bar even while the Agents tab is
                      shown; only the active one is visible. Each owns its own
                      fetch + optimistic update + live WS refresh. */}
                  <div style={{ display: sbTab === "tasks" ? "block" : "none" }}>
                    <RequestsInbox kind="task" onCountChange={setTaskCount} />
                  </div>
                  <div style={{ display: sbTab === "approvals" ? "block" : "none" }}>
                    <RequestsInbox kind="approval" onCountChange={setApprCount} />
                  </div>
                </div>
              </aside>

              {/* CHAT — reuses the EXACT canonical chat the Org-map SidePanel
                  renders (My Chat / Agent Comms sub-tabs, attachments, history,
                  delivery-mode handling), pointed at the platform agent. A thin
                  concierge-styled header keeps the Home look; the ChatTab body
                  below is identical to the map path so features can't drift. */}
              {chatId && chatNode ? (
                <section className={s.chat}>
                  <div className={s.chatHead}>
                    <div className={s.chAv}><IcChat /></div>
                    <div className={s.chMeta}>
                      <div className={s.chTitle}>{chatNode.data.name ?? (chatIsRoot ? "Org Concierge" : "Agent")}</div>
                      <div className={s.chSub}>
                        {(() => {
                          const online =
                            chatNode.data.status === "online" ||
                            chatNode.data.status === "degraded";
                          return (
                            <>
                              <span
                                className={s.sdot}
                                style={{ background: online ? "var(--green)" : "var(--grey)" }}
                              />
                              {online ? "online" : statusInfo(chatNode.data.status ?? "").label} · {chatIsRoot ? "platform agent" : (chatNode.data.role || "agent")}
                            </>
                          );
                        })()}
                      </div>
                    </div>
                  </div>
                  <div className={s.embedChat}>
                    {/* key=chatId remounts ChatTab on selection change so the
                        history/composer state never bleeds between agents. */}
                    <ChatTab key={chatId} workspaceId={chatId} data={chatNode.data} />
                  </div>
                </section>
              ) : (
                <section className={s.chat}>
                  <div className={s.greetWrap}>
                    <div className={s.greet}>
                      <span className={s.stamp}>✷</span> No platform agent yet
                    </div>
                  </div>
                </section>
              )}
            </div>

            {/* ORG MAP VIEW — the live canvas */}
            <div className={`${s.view} ${topView === "map" ? s.active : ""}`}>
              {topView === "map" && (
                <div className={s.canvasMount}>
                  <main aria-label="Agent canvas" style={{ position: "absolute", inset: 0 }}>
                    <Canvas />
                  </main>
                  <CommunicationOverlay />
                </div>
              )}
            </div>

            {/* SETTINGS VIEW */}
            <div className={`${s.view} ${topView === "settings" ? s.active : ""}`}>
              <div className={s.settingsScroll}>
                <div className={s.settingsInner}>
                  <div className={s.settingsHead}>
                    <h1>Settings</h1>
                    <p>
                      Org-level settings for the platform concierge. Configure the
                      concierge exactly like any workspace — config.yaml, plugins
                      and skills, container/compute, display, channels, schedule
                      and secrets — plus how it pays for model usage and org
                      identity.
                    </p>
                  </div>

                  {/* Two tabs instead of one long sheet: Platform agent
                      configuration vs Org & canvas settings. Reuses the same
                      .sbTabs purple-underline tab style as the Home sub-tabs. */}
                  <div className={s.sbTabs} role="tablist" aria-label="Settings sections">
                    <button
                      type="button"
                      role="tab"
                      data-testid="settings-tab-platform"
                      aria-selected={settingsTab === "platform"}
                      className={`${s.sbTab} ${settingsTab === "platform" ? s.active : ""}`}
                      onClick={() => setSettingsTab("platform")}
                    >
                      Platform agent configuration
                    </button>
                    <button
                      type="button"
                      role="tab"
                      data-testid="settings-tab-org"
                      aria-selected={settingsTab === "org"}
                      className={`${s.sbTab} ${settingsTab === "org" ? s.active : ""}`}
                      onClick={() => setSettingsTab("org")}
                    >
                      Org &amp; canvas settings
                    </button>
                  </div>

                  {/* Platform agent configuration — the FULL workspace tab UI
                      (Config, Plugins/Skills, Container, Display, Details,
                      Activity, Terminal, Channels, Schedule, Files, Memory,
                      Traces, Events, Audit), reusing the exact same
                      WorkspacePanelTabs the Org-map SidePanel renders so the two
                      surfaces can't drift. Pointed at the platform agent; the
                      panel owns its own local active-tab state so it doesn't
                      fight the map's node selection. */}
                  {settingsTab === "platform" && (
                    <div data-testid="settings-pane-platform" className={s.scard}>
                      <div className={s.scardHead}>
                        <div className={s.scardDesc}>
                          Update the concierge like any workspace: its config.yaml,
                          plugins &amp; skills, container/compute, display, channels,
                          schedule and more.
                        </div>
                      </div>
                      {platformRoot ? (
                        <div className={s.embedPanel}>
                          {/* idPrefix: this is a SECOND WorkspacePanelTabs instance —
                              without a namespace its tab/panel ids collide with the
                              map SidePanel's (#tab-chat duplicated → invalid HTML,
                              broken aria-controls, Playwright strict-mode failures). */}
                          <WorkspacePanelTabs key={platformRoot.id} node={platformRoot} defaultTab="config" idPrefix="concierge-" />
                        </div>
                      ) : (
                        <div className={s.scardDesc}>
                          No platform agent yet. Spin one up from Home to configure it.
                        </div>
                      )}
                    </div>
                  )}

                  {settingsTab === "org" && (
                    <div data-testid="settings-pane-org" className={s.scard}>
                      <div className={s.scardHead}>
                        <div className={s.scardDesc}>
                          Secrets, workspace tokens, org API keys and organization
                          identity. These also live behind the gear in the top bar.
                        </div>
                      </div>
                      {platformId && (
                        <div className={s.embedSettings}>
                          <SettingsTabs workspaceId={platformId} />
                        </div>
                      )}
                    </div>
                  )}
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
