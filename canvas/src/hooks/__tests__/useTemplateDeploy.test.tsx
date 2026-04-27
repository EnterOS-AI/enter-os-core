// @vitest-environment jsdom
/**
 * Tests for useTemplateDeploy — the shared preflight + POST + modal
 * hook used by TemplatePalette (sidebar) and EmptyState (welcome grid).
 *
 * Behavioural coverage for the three flows the hook owns:
 *   1. Happy path: preflight ok → POST /workspaces → onDeployed fires
 *   2. Preflight errors: network throw vs not-ok-with-missing-keys
 *      (different code paths — the throw must NOT strand `deploying`,
 *      see the inline comment in the SUT for the prior bug)
 *   3. Modal lifecycle: keys-added retries POST without re-running
 *      preflight; cancel closes without POST
 *
 * Issue: #2071 (Canvas test gaps follow-up).
 */
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
  type Mock,
} from "vitest";
import { act, render, cleanup, screen, fireEvent } from "@testing-library/react";
import { renderHook } from "@testing-library/react";
import type { Template } from "@/lib/deploy-preflight";

// ── Hoisted mocks ────────────────────────────────────────────────────────────
const { mockApiPost, mockCheckDeploySecrets, mockResolveRuntime } = vi.hoisted(
  () => ({
    mockApiPost: vi.fn(),
    mockCheckDeploySecrets: vi.fn(),
    mockResolveRuntime: vi.fn(),
  }),
);

vi.mock("@/lib/api", () => ({
  api: { post: mockApiPost },
}));

vi.mock("@/lib/deploy-preflight", async () => {
  // Re-export the real types; only swap the runtime functions.
  const actual = await vi.importActual<
    typeof import("@/lib/deploy-preflight")
  >("@/lib/deploy-preflight");
  return {
    ...actual,
    checkDeploySecrets: mockCheckDeploySecrets,
    resolveRuntime: mockResolveRuntime,
  };
});

// MissingKeysModal: render a minimal stand-in that exposes the two
// callbacks the hook wires up. The real modal pulls in radix + the
// secrets store, neither of which is relevant to this hook's behavior.
vi.mock("@/components/MissingKeysModal", () => ({
  MissingKeysModal: (props: {
    open: boolean;
    onKeysAdded: () => void;
    onCancel: () => void;
  }) =>
    props.open ? (
      <div data-testid="missing-keys-modal">
        <button data-testid="modal-keys-added" onClick={props.onKeysAdded}>
          keys added
        </button>
        <button data-testid="modal-cancel" onClick={props.onCancel}>
          cancel
        </button>
      </div>
    ) : null,
}));

// Import the hook AFTER the mocks are declared.
import { useTemplateDeploy } from "../useTemplateDeploy";

// ── Helpers ──────────────────────────────────────────────────────────────────

function makeTemplate(over: Partial<Template> = {}): Template {
  return {
    id: "claude-code-default",
    name: "Claude Code",
    description: "",
    tier: 1,
    model: "claude-sonnet-4-5",
    skills: [],
    skill_count: 0,
    runtime: "claude-code",
    models: [],
    required_env: [],
    ...over,
  };
}

beforeEach(() => {
  mockApiPost.mockReset();
  mockCheckDeploySecrets.mockReset();
  mockResolveRuntime.mockReset();
  // Default: identity-mapped runtime, preflight passes.
  mockResolveRuntime.mockImplementation((id: string) => id);
  mockCheckDeploySecrets.mockResolvedValue({
    ok: true,
    missingKeys: [],
    providers: [],
    runtime: "claude-code",
  });
  mockApiPost.mockResolvedValue({ id: "ws-new" });
});

afterEach(() => {
  cleanup();
});

// ── Tests ────────────────────────────────────────────────────────────────────

describe("useTemplateDeploy — happy path", () => {
  it("preflight ok → POST /workspaces → onDeployed fires with new id", async () => {
    const onDeployed = vi.fn();
    const { result } = renderHook(() => useTemplateDeploy({ onDeployed }));

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    expect(mockCheckDeploySecrets).toHaveBeenCalledTimes(1);
    expect(mockApiPost).toHaveBeenCalledWith(
      "/workspaces",
      expect.objectContaining({
        name: "Claude Code",
        template: "claude-code-default",
        tier: 1,
      }),
    );
    expect(onDeployed).toHaveBeenCalledWith("ws-new");
    expect(result.current.deploying).toBeNull();
    expect(result.current.error).toBeNull();
  });

  it("uses caller-supplied canvasCoords when provided", async () => {
    const canvasCoords = vi.fn(() => ({ x: 42, y: 99 }));
    const { result } = renderHook(() => useTemplateDeploy({ canvasCoords }));

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    expect(canvasCoords).toHaveBeenCalledTimes(1);
    expect(mockApiPost).toHaveBeenCalledWith(
      "/workspaces",
      expect.objectContaining({ canvas: { x: 42, y: 99 } }),
    );
  });

  it("falls back to random coords inside [100,500] × [100,400] when canvasCoords omitted", async () => {
    const { result } = renderHook(() => useTemplateDeploy());

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    const body = (mockApiPost as Mock).mock.calls[0]?.[1] as {
      canvas: { x: number; y: number };
    };
    expect(body.canvas.x).toBeGreaterThanOrEqual(100);
    expect(body.canvas.x).toBeLessThan(500);
    expect(body.canvas.y).toBeGreaterThanOrEqual(100);
    expect(body.canvas.y).toBeLessThan(400);
  });

  it("prefers template.runtime over resolveRuntime fallback", async () => {
    const { result } = renderHook(() => useTemplateDeploy());

    await act(async () => {
      await result.current.deploy(
        makeTemplate({ runtime: "hermes", id: "some-id" }),
      );
    });

    expect(mockResolveRuntime).not.toHaveBeenCalled();
    expect(mockCheckDeploySecrets).toHaveBeenCalledWith(
      expect.objectContaining({ runtime: "hermes" }),
    );
  });
});

describe("useTemplateDeploy — preflight failure modes", () => {
  it("preflight throw sets error and clears deploying (no stranded button)", async () => {
    mockCheckDeploySecrets.mockRejectedValueOnce(new Error("network down"));
    const { result } = renderHook(() => useTemplateDeploy());

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    expect(result.current.error).toBe("network down");
    expect(result.current.deploying).toBeNull();
    expect(mockApiPost).not.toHaveBeenCalled();
  });

  it("preflight not-ok opens the modal without firing POST", async () => {
    mockCheckDeploySecrets.mockResolvedValueOnce({
      ok: false,
      missingKeys: ["ANTHROPIC_API_KEY"],
      providers: [],
      runtime: "claude-code",
    });
    const onDeployed = vi.fn();

    const { result, rerender } = renderHook(() =>
      useTemplateDeploy({ onDeployed }),
    );

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    rerender();
    render(<>{result.current.modal}</>);
    expect(screen.getByTestId("missing-keys-modal")).toBeTruthy();
    expect(mockApiPost).not.toHaveBeenCalled();
    expect(onDeployed).not.toHaveBeenCalled();
    expect(result.current.deploying).toBeNull();
  });
});

describe("useTemplateDeploy — modal lifecycle", () => {
  it("'keys added' retries POST without re-running preflight", async () => {
    mockCheckDeploySecrets.mockResolvedValueOnce({
      ok: false,
      missingKeys: ["ANTHROPIC_API_KEY"],
      providers: [],
      runtime: "claude-code",
    });
    const onDeployed = vi.fn();
    const { result, rerender } = renderHook(() =>
      useTemplateDeploy({ onDeployed }),
    );

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });
    expect(mockCheckDeploySecrets).toHaveBeenCalledTimes(1);

    rerender();
    render(<>{result.current.modal}</>);

    // Click "keys added" — the hook should retry via executeDeploy
    // (which does NOT call preflight again).
    await act(async () => {
      fireEvent.click(screen.getByTestId("modal-keys-added"));
      // Let the fire-and-forget executeDeploy promise resolve.
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(mockCheckDeploySecrets).toHaveBeenCalledTimes(1); // still 1, not 2
    expect(mockApiPost).toHaveBeenCalledTimes(1);
    expect(onDeployed).toHaveBeenCalledWith("ws-new");
  });

  it("'cancel' closes the modal without firing POST", async () => {
    mockCheckDeploySecrets.mockResolvedValueOnce({
      ok: false,
      missingKeys: ["ANTHROPIC_API_KEY"],
      providers: [],
      runtime: "claude-code",
    });
    const { result, rerender } = renderHook(() => useTemplateDeploy());

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    rerender();
    const { rerender: renderRerender } = render(<>{result.current.modal}</>);
    expect(screen.getByTestId("missing-keys-modal")).toBeTruthy();

    await act(async () => {
      fireEvent.click(screen.getByTestId("modal-cancel"));
    });

    rerender();
    renderRerender(<>{result.current.modal}</>);
    expect(screen.queryByTestId("missing-keys-modal")).toBeNull();
    expect(mockApiPost).not.toHaveBeenCalled();
  });
});

describe("useTemplateDeploy — POST failure", () => {
  it("POST rejection sets error and clears deploying", async () => {
    mockApiPost.mockRejectedValueOnce(new Error("server 500"));
    const onDeployed = vi.fn();
    const { result } = renderHook(() => useTemplateDeploy({ onDeployed }));

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    expect(result.current.error).toBe("server 500");
    expect(result.current.deploying).toBeNull();
    expect(onDeployed).not.toHaveBeenCalled();
  });

  it("non-Error rejection still surfaces a message (defensive)", async () => {
    mockApiPost.mockRejectedValueOnce("plain string");
    const { result } = renderHook(() => useTemplateDeploy());

    await act(async () => {
      await result.current.deploy(makeTemplate());
    });

    expect(result.current.error).toBe("Deploy failed");
    expect(result.current.deploying).toBeNull();
  });
});
