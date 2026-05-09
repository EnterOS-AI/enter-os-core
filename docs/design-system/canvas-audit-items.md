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

### 🔴 HIGH: secrets-store.ts Performance
**File:** `canvas/src/stores/secrets-store.ts`
**Issue:** `getGrouped()` selector creates new objects every call (Object.fromEntries + arrays). Not memoized.
**Impact:** Causes unnecessary re-renders on frequent selector access.
**Fix needed:** Memoize the selector or use a proper Zustand selector pattern.

### 🟡 MEDIUM: Pre-commit Hook Verification
**Issue:** Pre-commit hook checks 'use client' on hook-using components but unclear if it actually fails on violations.
**Action:** Verify the hook is enforcing the rule correctly.

## Verified Findings

### Node Rendering ✅ (with notes)
- **Framework:** `@xyflow/react` (React Flow) — DOM-based, not SVG/Canvas
- **Node selection:** `aria-pressed` + border ring (`border-accent/70`) + shadow
- **Node drag:** React Flow native drag — mouse only, no keyboard alternative yet
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

### Keyboard Shortcuts ⚠️ PARTIAL
- Exists in `useKeyboardShortcuts.ts` but no `aria-describedby` on trigger buttons
- No dedicated keyboard shortcut help dialog
- **Gap:** Users can't discover shortcuts visually

### Focus Management ✅ (strong)
- Skip link → `#canvas-main` ✅
- `aria-label` on ReactFlow container ✅
- Focus trap in modals via Radix ✅
- Focus ring: `focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2 focus-visible:ring-offset-zinc-950`

### Accessibility Tree ⚠️ PARTIAL
- Canvas is in accessibility tree (React Flow DOM nodes)
- Node state changes not announced to screen readers (no `aria-live` region)
- Context menus announced via `role="menu"` ✅

### Context Menus ✅ (strong)
- `role="menu"`, `role="menuitem"`, `role="separator"` ✅
- `aria-label` with workspace name ✅
- ArrowUp/Down navigation with wrap-around ✅
- Escape + Tab close menu ✅
- Auto-focus first item on open ✅

### Drag and Drop ⚠️ PARTIAL
- **Mouse drag:** React Flow native
- **Drop target:** Visual indicator (`bg-emerald-950/40 border-emerald-400/60`) ✅
- **Keyboard alternative:** None — nodes repositioned only via mouse drag
- **Status:** Mouse-only. Keyboard users cannot rearrange nodes.

---

## Remaining Gaps (Priority Order)

| Priority | Item | Files | Status |
|----------|------|-------|--------|
| HIGH | Screen reader announcements for canvas state changes | Canvas.tsx | Not started |
| MEDIUM | Keyboard shortcut help dialog | useKeyboardShortcuts.ts | Not started |
| MEDIUM | Keyboard-accessible node drag | WorkspaceNode.tsx, useDragHandlers.ts | Not started |
| LOW | Edge anchor keyboard accessibility | A2AEdge.tsx | Not started |
| LOW | Node resize keyboard accessibility | WorkspaceNode.tsx (NodeResizer) | Not started |

---

*Verified 2026-05-09 by Core-UIUX against molecule-core/canvas/src/*
