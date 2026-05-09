# Canvas Architecture Audit — VERIFIED

> **Status:** VERIFIED — Cross-referenced against molecule-core/canvas/src/ (2026-05-09)
> **Author:** Core-FE (draft), Core-UIUX (verification)
> **Updated:** 2026-05-09 with architecture structure + known issues

## Canvas Stack (Verified)

| Technology | Version | Purpose |
|-----------|--------|---------|
| React Flow | `@xyflow/react` v12 | Node/edge rendering |
| Framework | Next.js 14 App Router | Routing, SSR |
| Styling | Tailwind v4 | CSS with custom properties |
| State | Zustand | Client state management |

## Directory Structure (Verified)

```
canvas/src/
├── components/
│   ├── Canvas.tsx           # Viewport management, ReactFlow wrapper
│   ├── Toolbar.tsx          # Add node/edge controls
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

## Verified Findings

### Node Rendering ✅ (with notes)
- **Framework:** `@xyflow/react` (React Flow) — DOM-based, not SVG/Canvas
- **Node selection:** `aria-pressed` + border ring (`border-accent/70`) + shadow
- **Node drag:** React Flow native drag + Arrow keys (10px/step, Shift 50px) — keyboard-accessible (PR #182) ✅
- **Node resize:** `NodeResizer` component visible on selected card, keyboard-inaccessible
- **Status:** Accessible via `aria-label` on node cards — "Alpha Workspace workspace — online"

### Edge Wiring ✅
- **Edge rendering:** React Flow SVG paths
- **Edge click target:** 1.5px stroke (CSS `stroke-width: 1.5 !important` in globals.css)
- **Edge creation:** React Flow drag-from-handle
- **Edge anchors:** Visible on hover (`hover:!bg-blue-400`), not keyboard accessible
- **Status:** Partial — mouse users only

### Canvas Controls ✅
- **Zoom:** React Flow Controls component (verify if keyboard accessible)
- **Pan:** Space+drag, mouse drag
- **Minimap:** Not present (MiniMap mocked as null in tests)
- **Status:** Basic keyboard support via viewport shortcuts

### Keyboard Shortcuts ✅ (strong)
- All shortcuts in `useKeyboardShortcuts.ts` with `inInput` guard ✅
- Global `?` shortcut opens `KeyboardShortcutsDialog` (PR #175) ✅
- Dialog: portal-based, aria-modal, focus trap, Escape close ✅
- Arrow keys move selected node 10px (50px with Shift) — keyboard node drag (this PR) ✅
- Hierarchy navigation (Enter/Shift+Enter), z-order (Cmd+]/[), zoom-to-team (Z) ✅

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
- **Keyboard alternative:** Arrow keys move selected node 10px per press (50px with Shift) (PR #182) ✅

---

## Remaining Gaps (Priority Order)

| Priority | Item | Files | Status |
|----------|------|-------|--------|
| ~~HIGH~~ | ~~Screen reader announcements for canvas state changes~~ | ~~Canvas.tsx, canvas-events.ts, canvas.ts~~ | ✅ Done — PR #172 |
| MEDIUM | Keyboard shortcut help dialog | useKeyboardShortcuts.ts | ✅ Done (PR #175) |
| MEDIUM | Keyboard-accessible node drag | WorkspaceNode.tsx, useDragHandlers.ts | ✅ Done (this PR) |
| LOW | Edge anchor keyboard accessibility | A2AEdge.tsx | Not started |
| LOW | Node resize keyboard accessibility | WorkspaceNode.tsx (NodeResizer) | Not started |

---

*Verified 2026-05-09 by Core-UIUX against molecule-core/canvas/src/*
*Updated 2026-05-09: screen reader announcements (PR #172) + keyboard shortcut dialog (PR #175) completed*
