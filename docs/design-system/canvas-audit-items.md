# Canvas Architecture Audit — VERIFIED

> **Status:** VERIFIED — Cross-referenced against molecule-core/canvas/src/ (2026-05-09)
> **Author:** Core-FE (draft), Core-UIUX (verification)
> **Updated:** 2026-05-10 with architecture structure + known issues + new test coverage (PR #205)

## Canvas Stack (Verified)

| Technology | Version | Purpose |
|-----------|--------|---------|
| React Flow | `@xyflow/react` v12 | Node/edge rendering |
| Framework | Next.js 15 App Router | Routing, SSR |
| Styling | Tailwind v4 | CSS with custom properties |
| State | Zustand | Client state management |

## Directory Structure (Verified)

```
canvas/src/
├── components/
│   ├── Canvas.tsx           # Viewport management, ReactFlow wrapper
│   ├── Toolbar.tsx          # Add node/edge controls
│   ├── KeyboardShortcutsDialog.tsx  # ? help dialog
│   ├── ContextMenu.tsx      # Right-click menu
│   ├── SidePanel.tsx        # Properties panel
│   ├── WorkspaceNode.tsx     # Node rendering
│   ├── A2AEdge.tsx          # Edge rendering
│   └── [tests]/             # Accessibility + component tests
├── stores/
│   └── secrets-store.ts     # ⚠️ getGrouped() performance issue
├── hooks/
│   ├── useSocketEvent.ts
│   ├── useTemplateDeploy.tsx
│   └── useWorkspaceName.ts
└── lib/
    ├── api.ts
    ├── auth.ts
    ├── canvas-actions.ts
    ├── design-tokens.ts     # STATUS_CONFIG, TIER_CONFIG
    ├── theme.ts
    └── theme-provider.tsx   # ThemeProvider, useTheme()

## Known Issues

### ✅ MEDIUM: secrets-store.ts Performance (mitigated)
**File:** `canvas/src/stores/secrets-store.ts`
**Issue:** `getGrouped()` selector creates new objects every call. Not memoized.
**Impact:** Mitigated — `SecretsTab.tsx` wraps the call in `useMemo`, so no active re-render issues in the single consumer. The store-level fix (memoizing `getGrouped` itself) is optional but low priority now.

### 🟡 MEDIUM: Pre-commit Hook Verification
**Issue:** Pre-commit hook checks 'use client' on hook-using components but unclear if it actually fails on violations.
**Action:** Verify the hook is enforcing the rule correctly.

### ✅ MEDIUM: text-ink-soft WCAG AA contrast (fixed)
**File:** `canvas/src/app/globals.css` + all canvas components
**Issue:** `--color-ink-soft` (#8d92a0) on dark zinc (#0e1014) = ~2.2:1 contrast,
below the WCAG 2.1 AA minimum of 4.5:1 for normal text.
**Impact:** Used in 261 instances across 52 files (captions, group titles, hints).
**Fix:** Replaced `text-ink-soft` → `text-ink-mid` (7.6:1) across all canvas source.
PR: `fix/ink-soft-wcag-contrast`.

## Verified Findings

### Node Rendering ✅ (with notes)
- **Framework:** `@xyflow/react` (React Flow) — DOM-based, not SVG/Canvas
- **Node selection:** `aria-pressed` + border ring (`border-accent/70`) + shadow
- **Node drag:** React Flow native drag + Arrow keys (10px/step, Shift 50px) — keyboard-accessible (PR #182) ✅
- **Node resize:** `NodeResizer` component visible on selected card; `Cmd/Ctrl+Arrow` keys resize (↑↓ height, ←→ width, 10px/step, Shift 2px) — keyboard-accessible ✅
- **Status:** Accessible via `aria-label` on node cards — "Alpha Workspace workspace — online"

### Edge Wiring ✅
- **Edge rendering:** React Flow SVG paths
- **Edge click target:** 1.5px stroke (CSS `stroke-width: 1.5 !important` in globals.css)
- **Edge creation:** React Flow drag-from-handle (mouse); keyboard via handle Enter/Space
- **Edge anchors:** Target handle (top): `Enter/Space` extracts node from parent. Source handle (bottom): `Enter/Space` nests selected node into this node. Both have `tabIndex=0`, `role="button"`, descriptive `aria-label`, and a blue focus ring ✅
- **Status:** Mouse + keyboard — keyboard users can nest and un-nest without a mouse

### Canvas Controls ✅
- **Zoom:** React Flow Controls component (zoom in/out/fit — each button has aria-label; keyboard-accessible) ✅
- **Pan:** Space+drag, mouse drag
- **Minimap:** Present with status-colored nodes (online=green, offline=zinc, degraded=amber, failed=red, provisioning=sky) ✅
- **Status:** Basic keyboard support via viewport shortcuts

### Keyboard Shortcuts ✅ (strong)
- All shortcuts in `useKeyboardShortcuts.ts` with `inInput` guard ✅
- Global `?` shortcut opens `KeyboardShortcutsDialog` (PR #175) ✅
- Dialog: portal-based, aria-modal, focus trap, Escape close ✅
- Arrow keys move selected node 10px (50px with Shift) — keyboard node drag (PR #182) ✅
- `Cmd/Ctrl+Arrow` resize selected node (↑↓ height, ←→ width, 10px, Shift 2px) ✅
- Hierarchy navigation (Enter/Shift+Enter), z-order (Cmd+]/[), zoom-to-team (Z) ✅
- Toolbar help dialog ("Shortcuts & tips") documents all shortcuts + mouse interactions ✅

### Focus Management ✅ (strong)
- Skip link → `#canvas-main` ✅
- `aria-label` on ReactFlow container ✅
- Focus trap in modals via Radix ✅
- Focus ring: `focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-950`

### Accessibility Tree ✅
- Canvas is in accessibility tree (React Flow DOM nodes)
- Node state changes announced via `aria-live="polite"` region (PR #172) ✅
- Context menus announced via `role="menu"` ✅

### Context Menus ✅ (strong)
- `role="menu"`, `role="menuitem"`, `role="separator"` ✅
- `aria-label` with workspace name ✅
- ArrowUp/Down navigation with wrap-around ✅
- Escape + Tab close menu ✅
- Auto-focus first item on open ✅

### Drag and Drop ✅
- **Mouse drag:** React Flow native
- **Drop target:** Visual indicator (`bg-emerald-950/40 border-emerald-400/60`) ✅
- **Keyboard alternative:** Arrow-key nudge via `useKeyboardShortcuts` (PR #182) ✅
- **Status:** Full — mouse and keyboard users can reposition nodes.

---

## Remaining Gaps (Priority Order)

| Priority | Item | Files | Status |
|----------|------|-------|--------|
| ~~HIGH~~ | ~~Screen reader announcements for canvas state changes~~ | ~~Canvas.tsx, canvas-events.ts, canvas.ts~~ | ✅ Done — PR #172 |
| MEDIUM | Keyboard shortcut help dialog | useKeyboardShortcuts.ts | ✅ Done (PR #175) |
| MEDIUM | Keyboard-accessible node drag | WorkspaceNode.tsx, useDragHandlers.ts | ✅ Done (PR #182) |
| LOW | Keyboard-accessible edge anchors | A2AEdge.tsx, WorkspaceNode.tsx | ✅ Done (PR #190) |
| LOW | Keyboard-accessible node resize | useKeyboardShortcuts.ts, WorkspaceNode.tsx | ✅ Done (PR #192) |

---

*Verified 2026-05-09 by Core-UIUX against molecule-core/canvas/src/*
*Updated 2026-05-10: keyboard shortcut dialog (PR #175) + keyboard node drag (PR #182) + keyboard edge anchors (PR #190) + keyboard node resize (PR #192) + screen reader announcements (PR #172) + text-ink-soft WCAG AA fix + Next.js 15.5.15 + component test coverage (PR #205: Tooltip, Legend, TermsGate, ApprovalBanner)*
