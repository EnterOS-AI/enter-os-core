// @vitest-environment jsdom
/**
 * MobileApp route-state contract.
 *
 * The mobile shell uses local React state (not URL routing) for
 * navigation between the 6 screens. This test pins the back-stack
 * shape so a future refactor can't silently regress:
 *
 *   home  →(open agent)→ detail
 *   detail →(open chat)→ chat       chat  →(back)→ detail
 *                                   detail →(back)→ home
 *
 *   home / canvas / comms / me — reachable via the bottom tab bar.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

beforeEach(() => {
  // URL state persists across tests in jsdom — reset to a clean slate
  // so each test starts on the home route regardless of what the
  // previous test pushed onto the history stack.
  window.history.replaceState(null, "", "/");
});

afterEach(() => {
  cleanup();
});

// Mock the theme provider — MobileApp reads resolvedTheme to pick a
// palette; for routing we don't care which one, light is fine.
vi.mock("@/lib/theme-provider", () => ({
  useTheme: () => ({ theme: "light", resolvedTheme: "light", setTheme: vi.fn() }),
}));

// Stub each screen to a sentinel that exposes the props MobileApp passes
// in. The whole point is to verify the routing handoff, not the screens
// themselves — those have their own tests.
vi.mock("../MobileHome", () => ({
  MobileHome: ({ onOpen, onSpawn }: { onOpen: (id: string) => void; onSpawn: () => void }) => (
    <div>
      <span data-testid="screen">home</span>
      <button onClick={() => onOpen("ws-42")}>open-ws-42</button>
      <button onClick={onSpawn}>open-spawn</button>
    </div>
  ),
}));
vi.mock("../MobileCanvas", () => ({
  MobileCanvas: () => <span data-testid="screen">canvas</span>,
}));
vi.mock("../MobileDetail", () => ({
  MobileDetail: ({
    agentId,
    onBack,
    onChat,
  }: {
    agentId: string;
    onBack: () => void;
    onChat: () => void;
  }) => (
    <div>
      <span data-testid="screen">detail:{agentId}</span>
      <button onClick={onBack}>detail-back</button>
      <button onClick={onChat}>detail-open-chat</button>
    </div>
  ),
}));
vi.mock("../MobileChat", () => ({
  MobileChat: ({ agentId, onBack }: { agentId: string; onBack: () => void }) => (
    <div>
      <span data-testid="screen">chat:{agentId}</span>
      <button onClick={onBack}>chat-back</button>
    </div>
  ),
}));
vi.mock("../MobileComms", () => ({
  MobileComms: () => <span data-testid="screen">comms</span>,
}));
vi.mock("../MobileMe", () => ({
  MobileMe: () => <span data-testid="screen">me</span>,
}));
vi.mock("../MobileSpawn", () => ({
  MobileSpawn: ({ onClose }: { onClose: () => void }) => (
    <div>
      <span data-testid="spawn-sheet">spawn</span>
      <button onClick={onClose}>spawn-close</button>
    </div>
  ),
}));

// MobileApp's shared TabBar is the user's gateway to the Canvas / Comms /
// Me screens. Rather than depend on its visual icon set we expose a
// label-based stub so the test can call onChange directly.
vi.mock("../components", async () => {
  const actual = await vi.importActual<typeof import("../components")>("../components");
  type TabId = "agents" | "canvas" | "comms" | "me";
  return {
    ...actual,
    TabBar: ({ onChange }: { active: TabId; onChange: (id: TabId) => void }) => (
      <div data-testid="tab-bar">
        {(["agents", "canvas", "comms", "me"] as const).map((id) => (
          <button key={id} onClick={() => onChange(id)}>
            tab-{id}
          </button>
        ))}
      </div>
    ),
  };
});

import { MobileApp } from "../MobileApp";

const visibleScreen = () =>
  Array.from(document.querySelectorAll('[data-testid="screen"]'))
    .map((el) => el.textContent ?? "")
    .filter(Boolean);

describe("MobileApp — route state", () => {
  it("starts on the home screen", () => {
    render(<MobileApp />);
    expect(visibleScreen()).toEqual(["home"]);
  });

  it("home → open agent → detail (passes agentId through)", () => {
    render(<MobileApp />);
    fireEvent.click(screen.getByText("open-ws-42"));
    expect(visibleScreen()).toEqual(["detail:ws-42"]);
  });

  it("detail → open chat → chat (carries the same agentId)", () => {
    render(<MobileApp />);
    fireEvent.click(screen.getByText("open-ws-42"));
    fireEvent.click(screen.getByText("detail-open-chat"));
    expect(visibleScreen()).toEqual(["chat:ws-42"]);
  });

  it("chat back returns to detail (NOT to home — preserves the back-stack)", () => {
    render(<MobileApp />);
    fireEvent.click(screen.getByText("open-ws-42"));
    fireEvent.click(screen.getByText("detail-open-chat"));
    fireEvent.click(screen.getByText("chat-back"));
    expect(visibleScreen()).toEqual(["detail:ws-42"]);
  });

  it("detail back returns to home", () => {
    render(<MobileApp />);
    fireEvent.click(screen.getByText("open-ws-42"));
    fireEvent.click(screen.getByText("detail-back"));
    expect(visibleScreen()).toEqual(["home"]);
  });

  it("hides the tab bar on chat (per design — composer reclaims that space)", () => {
    render(<MobileApp />);
    expect(screen.queryByTestId("tab-bar")).not.toBeNull();
    fireEvent.click(screen.getByText("open-ws-42"));
    expect(screen.queryByTestId("tab-bar")).not.toBeNull(); // detail
    fireEvent.click(screen.getByText("detail-open-chat"));
    expect(screen.queryByTestId("tab-bar")).toBeNull(); // chat
  });

  it("tab bar switches the four primary screens (Agents / Canvas / Comms / Me)", () => {
    render(<MobileApp />);
    fireEvent.click(screen.getByText("tab-canvas"));
    expect(visibleScreen()).toEqual(["canvas"]);
    fireEvent.click(screen.getByText("tab-comms"));
    expect(visibleScreen()).toEqual(["comms"]);
    fireEvent.click(screen.getByText("tab-me"));
    expect(visibleScreen()).toEqual(["me"]);
    fireEvent.click(screen.getByText("tab-agents"));
    expect(visibleScreen()).toEqual(["home"]);
  });

  it("spawn sheet overlays from anywhere, closes on dismiss", () => {
    render(<MobileApp />);
    expect(screen.queryByTestId("spawn-sheet")).toBeNull();
    fireEvent.click(screen.getByText("open-spawn"));
    expect(screen.queryByTestId("spawn-sheet")).not.toBeNull();
    fireEvent.click(screen.getByText("spawn-close"));
    expect(screen.queryByTestId("spawn-sheet")).toBeNull();
  });

  it("seeds initial route from ?m= and ?a= so deep links open the right screen", () => {
    window.history.replaceState(null, "", "/?m=detail&a=ws-99");
    render(<MobileApp />);
    expect(visibleScreen()).toEqual(["detail:ws-99"]);
  });

  it("collapses ?m=detail without ?a to home (detail without an agent is meaningless)", () => {
    window.history.replaceState(null, "", "/?m=detail");
    render(<MobileApp />);
    expect(visibleScreen()).toEqual(["home"]);
  });

  it("syncs in-app navigation to the URL so browser back leaves the mobile stack", () => {
    render(<MobileApp />);
    expect(window.location.search).toBe("");
    fireEvent.click(screen.getByText("open-ws-42"));
    expect(window.location.search).toBe("?m=detail&a=ws-42");
    fireEvent.click(screen.getByText("detail-open-chat"));
    expect(window.location.search).toBe("?m=chat&a=ws-42");
  });

  it("popstate (back button) restores the previous route", () => {
    render(<MobileApp />);
    fireEvent.click(screen.getByText("open-ws-42"));
    fireEvent.click(screen.getByText("detail-open-chat"));
    // Simulate browser back: rewind URL ourselves, then dispatch popstate.
    window.history.replaceState(null, "", "/?m=detail&a=ws-42");
    fireEvent.popState(window);
    expect(visibleScreen()).toEqual(["detail:ws-42"]);
  });
});
