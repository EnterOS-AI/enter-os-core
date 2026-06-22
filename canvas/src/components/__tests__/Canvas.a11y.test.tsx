// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
});

// ── Mock @xyflow/react ────────────────────────────────────────────────────────
vi.mock("@xyflow/react", () => {
  const ReactFlow = ({
    children,
    "aria-label": ariaLabel,
  }: {
    children?: React.ReactNode;
    "aria-label"?: string;
  }) => (
    <div role="application" data-testid="react-flow" aria-label={ariaLabel}>
      {children}
    </div>
  );
  return {
    __esModule: true,
    default: ReactFlow,
    ReactFlow,
    ReactFlowProvider: ({ children }: { children?: React.ReactNode }) => (
      <>{children}</>
    ),
    Background: () => null,
    Controls: () => null,
    MiniMap: () => null,
    BackgroundVariant: { Dots: "dots" },
    useReactFlow: () => ({
      fitView: vi.fn(),
      setViewport: vi.fn(),
      getIntersectingNodes: vi.fn(() => []),
      fitBounds: vi.fn(),
      setCenter: vi.fn(),
    }),
    applyNodeChanges: vi.fn((_: unknown, nodes: unknown) => nodes),
    useStore: vi.fn(() => ({ width: 800, height: 600 })),
  };
});

// ── Mock the canvas store ─────────────────────────────────────────────────────
const mockStoreState = {
  nodes: [],
  edges: [],
  selectedNodeId: null,
  panelTab: "chat",
  dragOverNodeId: null,
  contextMenu: null,
  viewport: { x: 0, y: 0, zoom: 1 },
  searchOpen: false,
  onNodesChange: vi.fn(),
  savePosition: vi.fn(),
  saveViewport: vi.fn(),
  selectNode: vi.fn(),
  openContextMenu: vi.fn(),
  closeContextMenu: vi.fn(),
  setDragOverNode: vi.fn(),
  nestNode: vi.fn(),
  isDescendant: vi.fn(() => false),
  setSearchOpen: vi.fn(),
  wsStatus: "connected" as const,
  setWsStatus: vi.fn(),
  a2aEdges: [],
  setA2AEdges: vi.fn(),
  showA2AEdges: false,
  setShowA2AEdges: vi.fn(),
  setPanelTab: vi.fn(),
  selectedNodeIds: new Set<string>(),
  clearSelection: vi.fn(),
  toggleNodeSelection: vi.fn(),
  deletingIds: new Set<string>(),
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: typeof mockStoreState) => unknown) =>
      selector(mockStoreState)
    ),
    { getState: () => mockStoreState }
  ),
}));

// ── Mock the socket store ─────────────────────────────────────────────────────
vi.mock("@/store/socket", () => ({
  connectSocket: vi.fn(),
  disconnectSocket: vi.fn(),
}));

// ── Mock all heavy child components to null ───────────────────────────────────
vi.mock("../Toolbar", () => ({ Toolbar: () => null }));
vi.mock("../SidePanel", () => ({ SidePanel: () => null }));
vi.mock("../EmptyState", () => ({ EmptyState: () => null }));
vi.mock("../ContextMenu", () => ({ ContextMenu: () => null }));
vi.mock("../SearchDialog", () => ({ SearchDialog: () => null }));
vi.mock("../ConfirmDialog", () => ({ ConfirmDialog: () => null }));
vi.mock("../TemplatePalette", () => ({ TemplatePalette: () => null }));
vi.mock("../OnboardingWizard", () => ({ OnboardingWizard: () => null }));
vi.mock("../ApprovalBanner", () => ({ ApprovalBanner: () => null }));
vi.mock("../BundleDropZone", () => ({ BundleDropZone: () => null }));
vi.mock("../CreateWorkspaceDialog", () => ({ CreateWorkspaceButton: () => null }));
vi.mock("../settings", () => ({
  SettingsPanel: () => null,
  DeleteConfirmDialog: () => null,
}));
vi.mock("../Toaster", () => ({ Toaster: () => null }));
vi.mock("../WorkspaceNode", () => ({ WorkspaceNode: () => null }));
vi.mock("../ProvisioningTimeout", () => ({
  ProvisioningTimeout: () => (
    <div data-testid="provisioning-timeout-sentinel" />
  ),
}));
vi.mock("../BatchActionBar", () => ({ BatchActionBar: () => null }));

// ── Import the component under test AFTER mocks ───────────────────────────────
import { Canvas } from "../Canvas";

describe("Canvas — accessibility landmarks", () => {
  it("renders a <main> landmark with id='canvas-main'", () => {
    render(<Canvas />);
    const main = screen.getByRole("main");
    expect(main).toBeTruthy();
    expect(main.id).toBe("canvas-main");
  });

  it("renders a skip-to-content link pointing at #canvas-main", () => {
    render(<Canvas />);
    const skipLink = document.querySelector('a[href="#canvas-main"]');
    expect(skipLink).toBeTruthy();
    expect(skipLink?.textContent?.trim()).toBe("Skip to canvas");
  });

  it("ReactFlow wrapper receives aria-label describing the canvas", () => {
    render(<Canvas />);
    const flow = document.querySelector('[data-testid="react-flow"]');
    expect(flow?.getAttribute("aria-label")).toBe(
      "Molecule AI workspace canvas"
    );
  });

  it("skip link appears before <main> in the DOM", () => {
    render(<Canvas />);
    const body = document.body;
    const skipLink = body.querySelector('a[href="#canvas-main"]');
    const main = body.querySelector("main");
    expect(skipLink).toBeTruthy();
    expect(main).toBeTruthy();
    // skip link must come before main in the DOM order
    const position = skipLink!.compareDocumentPosition(main!);
    expect(position & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

// ── Fix #833: ProvisioningTimeout is mounted in the Canvas tree ───────────────
describe("Canvas — ProvisioningTimeout integration (issue #833)", () => {
  it("renders ProvisioningTimeout in the component tree", () => {
    render(<Canvas />);
    expect(
      document.querySelector(
        '[data-testid="provisioning-timeout-sentinel"]'
      )
    ).toBeTruthy();
  });
});
