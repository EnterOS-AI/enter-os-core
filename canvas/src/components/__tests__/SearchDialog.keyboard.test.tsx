// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

afterEach(cleanup);

// ── Mock store data ───────────────────────────────────────────────────────────
const setOpen = vi.fn();
const selectNode = vi.fn();
const setPanelTab = vi.fn();

const mockNodes = [
  {
    id: "ws-1",
    data: {
      name: "Alpha",
      status: "online",
      tier: 1,
      role: "dev",
      parentId: null,
    },
  },
  {
    id: "ws-2",
    data: {
      name: "Beta",
      status: "offline",
      tier: 2,
      role: "ops",
      parentId: null,
    },
  },
  {
    id: "ws-3",
    data: {
      name: "Gamma",
      status: "provisioning",
      tier: 1,
      role: "qa",
      parentId: null,
    },
  },
];

const mockStore = {
  searchOpen: true,
  setSearchOpen: setOpen,
  nodes: mockNodes as typeof mockNodes,
  selectNode,
  setPanelTab,
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn(
    (selector: (s: typeof mockStore) => unknown) => selector(mockStore)
  ),
}));

// ── Component under test — imported AFTER mocks ───────────────────────────────
import { SearchDialog } from "../SearchDialog";

describe("SearchDialog — keyboard accessibility", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockStore.searchOpen = true;
    mockStore.nodes = mockNodes;
  });

  it("renders with role='dialog' and aria-modal='true'", () => {
    render(<SearchDialog />);
    const dialog = screen.getByRole("dialog");
    expect(dialog).toBeTruthy();
    expect(dialog.getAttribute("aria-modal")).toBe("true");
  });

  it("dialog has an aria-label", () => {
    render(<SearchDialog />);
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("aria-label")).toBeTruthy();
  });

  it("search input has role='combobox'", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    expect(input).toBeTruthy();
  });

  it("results container has role='listbox'", () => {
    render(<SearchDialog />);
    const listbox = screen.getByRole("listbox");
    expect(listbox).toBeTruthy();
  });

  it("result items have role='option'", () => {
    render(<SearchDialog />);
    const options = screen.getAllByRole("option");
    expect(options.length).toBe(3);
  });

  it("ArrowDown sets aria-selected='true' on the first option", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("true");
    expect(options[1].getAttribute("aria-selected")).toBe("false");
  });

  it("ArrowDown twice sets aria-selected='true' on the second option", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowDown" });
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("false");
    expect(options[1].getAttribute("aria-selected")).toBe("true");
  });

  it("ArrowDown clamps at the last option — does not wrap", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    // Press ArrowDown 5 times with only 3 items — should stop at index 2
    for (let i = 0; i < 5; i++) {
      fireEvent.keyDown(input, { key: "ArrowDown" });
    }
    const options = screen.getAllByRole("option");
    expect(options[2].getAttribute("aria-selected")).toBe("true");
    // first two must not be selected
    expect(options[0].getAttribute("aria-selected")).toBe("false");
    expect(options[1].getAttribute("aria-selected")).toBe("false");
  });

  it("ArrowUp from index 0 stays at 0 (Math.max clamp)", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.keyDown(input, { key: "ArrowDown" }); // focusedIndex → 0
    fireEvent.keyDown(input, { key: "ArrowUp" });   // Math.max(0-1, 0) = 0, stays at 0
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("true");
  });

  it("Enter key selects the currently focused option", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.keyDown(input, { key: "ArrowDown" }); // focus index 0 (ws-1)
    fireEvent.keyDown(input, { key: "Enter" });
    expect(selectNode).toHaveBeenCalledWith("ws-1");
  });

  it("Enter at focusedIndex=-1 does not select anything", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    // No ArrowDown — focusedIndex is -1
    fireEvent.keyDown(input, { key: "Enter" });
    expect(selectNode).not.toHaveBeenCalled();
  });

  it("typing a query that matches auto-highlights the first result", () => {
    // Replaces the older "resets to -1" assertion. New behavior: a query
    // with at least one match pins the highlight to row 0 so Enter picks
    // a result instead of being a no-op. Empty-query case is covered by
    // "Enter at focusedIndex=-1 does not select anything" above.
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "Alpha" } });
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("true");
    // Enter on the auto-highlighted match should select it without
    // needing a manual ArrowDown first.
    fireEvent.keyDown(input, { key: "Enter" });
    expect(selectNode).toHaveBeenCalledWith("ws-1");
  });

  it("typing a query that matches NOTHING resets focusedIndex to -1", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.keyDown(input, { key: "ArrowDown" }); // focusedIndex → 0
    fireEvent.change(input, { target: { value: "zzz-no-match" } });
    // No options remain, so nothing to assert on aria-selected directly —
    // the empty-state message takes over. But Enter should be a no-op.
    fireEvent.keyDown(input, { key: "Enter" });
    expect(selectNode).not.toHaveBeenCalled();
  });

  it("aria-activedescendant matches the focused option's id", () => {
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.keyDown(input, { key: "ArrowDown" }); // focusedIndex → 0 (ws-1)
    expect(input.getAttribute("aria-activedescendant")).toBe(
      "search-result-ws-1"
    );
  });

  it("returns null when searchOpen is false", () => {
    mockStore.searchOpen = false;
    const { container } = render(<SearchDialog />);
    expect(container.firstChild).toBeNull();
  });
});
