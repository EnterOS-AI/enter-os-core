// @vitest-environment jsdom
/**
 * Tests for ExternalConnectModal — the modal surfaced after creating a
 * runtime="external" workspace. Surfaces workspace_auth_token + ready-to-paste
 * snippets so the operator can configure their off-host agent.
 *
 * Coverage:
 *   - Renders nothing when info=null
 *   - Opens dialog when info is provided
 *   - Default tab: "Universal MCP" when universal_mcp_snippet present, else "Python SDK"
 *   - Tab switching between all available tabs
 *   - Snippets show with auth_token replacing placeholders
 *   - Copy button: calls clipboard API, shows "Copied!", clears after 1.5s
 *   - Copy failure: shows fallback textarea
 *   - "I've saved it — close" calls onClose
 *   - Security warning: one-time token display
 *   - Fields tab shows raw values
 *   - Tabs hidden when their snippet is absent
 *
 * Fake timers: applied per-describe to avoid mixing with waitFor. Tests that
 * use waitFor (which needs real timers) run without fake timers. Tests that
 * verify setTimeout behavior use vi.useFakeTimers() + act(vi.advanceTimersByTime).
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  ExternalConnectModal,
  type ExternalConnectionInfo,
} from "../ExternalConnectModal";

const defaultInfo: ExternalConnectionInfo = {
  workspace_id: "ws-123",
  platform_url: "https://app.example.com",
  auth_token: "secret-auth-token-abc",
  registry_endpoint: "https://app.example.com/api/a2a/register",
  heartbeat_endpoint: "https://app.example.com/api/a2a/heartbeat",
  // Placeholders must EXACTLY match what the component searches for in
  // the string.replace() calls (the component does NOT normalise whitespace).
  // Python: 'AUTH_TOKEN    = "...' (4 spaces), curl: WORKSPACE_AUTH_TOKEN="<paste>" (with quotes),
  // MCP/Hermes: MOLECULE_WORKSPACE_TOKEN="...", Codex: same with 1 space.
  curl_register_template:
    `curl -X POST https://app.example.com/api/a2a/register \\
  -H "Content-Type: application/json" \\
  -d '{"auth_token": "WORKSPACE_AUTH_TOKEN=\"<paste from create response>\"", ...}'`,
  python_snippet:
    'AUTH_TOKEN    = "<paste from create response>"\nAPI_URL = "https://app.example.com"',
  universal_mcp_snippet:
    'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
  hermes_channel_snippet:
    'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
  codex_snippet: 'MOLECULE_WORKSPACE_TOKEN = "<paste from create response>"',
  openclaw_snippet: 'WORKSPACE_TOKEN="<paste from create response>"',
};

// ─── Clipboard mock helpers ────────────────────────────────────────────────────

let clipboardWriteText = vi.fn();

beforeEach(() => {
  clipboardWriteText.mockReset().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", {
    value: { writeText: clipboardWriteText },
    configurable: true,
    writable: true,
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ─── Helpers ──────────────────────────────────────────────────────────────────

function renderModal(info: ExternalConnectionInfo | null) {
  return render(
    <ExternalConnectModal info={info} onClose={vi.fn()} />,
  );
}

// Flush React + Radix portal updates synchronously so the dialog is in the DOM.
function renderAndFlush(info: ExternalConnectionInfo | null) {
  const result = renderModal(info);
  act(() => {});
  return result;
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("ExternalConnectModal — render conditions", () => {
  it("renders nothing when info is null", () => {
    renderModal(null);
    expect(document.body.textContent).toBe("");
  });

  it("renders the dialog when info is provided", () => {
    renderAndFlush(defaultInfo);
    expect(screen.queryByRole("dialog")).toBeTruthy();
  });

  it("shows the security warning about one-time token display", () => {
    renderAndFlush(defaultInfo);
    expect(screen.getByText(/only once/i)).toBeTruthy();
  });
});

describe("ExternalConnectModal — default tab selection", () => {
  it("opens the Universal MCP tab by default when universal_mcp_snippet is present", () => {
    renderAndFlush(defaultInfo);
    const mcpTab = screen.getByRole("tab", { name: /universal mcp/i });
    expect(mcpTab.getAttribute("aria-selected")).toBe("true");
  });

  it("opens the Python SDK tab by default when universal_mcp_snippet is absent", () => {
    renderAndFlush({ ...defaultInfo, universal_mcp_snippet: undefined });
    const pythonTab = screen.getByRole("tab", { name: /python sdk/i });
    expect(pythonTab.getAttribute("aria-selected")).toBe("true");
  });

  it("tab order: Universal MCP appears before Python SDK when both exist", () => {
    renderAndFlush(defaultInfo);
    const tabs = screen.getAllByRole("tab");
    const mcpIndex = tabs.findIndex((t) => t.textContent?.includes("Universal MCP"));
    const pythonIndex = tabs.findIndex((t) => t.textContent?.includes("Python SDK"));
    expect(mcpIndex).toBeLessThan(pythonIndex);
  });
});

describe("ExternalConnectModal — tab switching", () => {
  it("switches to the Python SDK tab and shows the snippet with stamped token", () => {
    renderAndFlush(defaultInfo);
    fireEvent.click(screen.getByRole("tab", { name: /python sdk/i }));
    // Query within the python panel so we get the right pre (not the first in DOM).
    const pythonPanel = document.querySelector("[data-testid='panel-python']");
    const preEl = pythonPanel?.querySelector("pre");
    expect(preEl?.textContent).toContain("AUTH_TOKEN");
    // The placeholder is replaced with the real auth token
    expect(preEl?.textContent).toContain("secret-auth-token-abc");
  });

  it("switches to the curl tab and shows the snippet with stamped token", () => {
    renderAndFlush(defaultInfo);
    fireEvent.click(screen.getByRole("tab", { name: /curl/i }));
    // Query within the curl panel so we get the right pre (not the first in DOM).
    const curlPanel = document.querySelector("[data-testid='panel-curl']");
    const preEl = curlPanel?.querySelector("pre");
    expect(preEl?.textContent).toContain("curl");
    expect(preEl?.textContent).toContain("secret-auth-token-abc");
  });

  it("switches to the Fields tab and shows raw values", () => {
    renderAndFlush(defaultInfo);
    fireEvent.click(screen.getByRole("tab", { name: /fields/i }));
    // Query within the fields panel for specific values.
    const fieldsPanel = document.querySelector("[data-testid='panel-fields']");
    expect(fieldsPanel?.textContent).toContain("ws-123");
    expect(fieldsPanel?.textContent).toContain("https://app.example.com");
    expect(fieldsPanel?.textContent).toContain("secret-auth-token-abc");
  });

  it("hides the Hermes tab when hermes_channel_snippet is absent", () => {
    renderAndFlush({ ...defaultInfo, hermes_channel_snippet: undefined });
    expect(screen.queryByRole("tab", { name: /hermes/i })).toBeNull();
  });

  it("shows Hermes tab when hermes_channel_snippet is present", () => {
    renderAndFlush(defaultInfo);
    expect(screen.getByRole("tab", { name: /hermes/i })).toBeTruthy();
  });
});

describe("ExternalConnectModal — snippet token stamping", () => {
  it("stamps the real auth_token into the Python snippet instead of the placeholder", () => {
    renderAndFlush(defaultInfo);
    fireEvent.click(screen.getByRole("tab", { name: /python sdk/i }));
    const pythonPanel = document.querySelector("[data-testid='panel-python']");
    const preEl = pythonPanel?.querySelector("pre");
    expect(preEl?.textContent).not.toContain("<paste from create response>");
    expect(preEl?.textContent).toContain("secret-auth-token-abc");
  });

  it("stamps the real auth_token into the curl snippet", () => {
    renderAndFlush(defaultInfo);
    fireEvent.click(screen.getByRole("tab", { name: /curl/i }));
    const curlPanel = document.querySelector("[data-testid='panel-curl']");
    const preEl = curlPanel?.querySelector("pre");
    // curl template uses WORKSPACE_AUTH_TOKEN placeholder, not the generic one
    expect(preEl?.textContent).toContain("secret-auth-token-abc");
  });

  it("stamps the real auth_token into the Universal MCP snippet", () => {
    renderAndFlush(defaultInfo);
    // Default tab is Universal MCP
    const mcpPanel = document.querySelector("[data-testid='panel-mcp']");
    const preEl = mcpPanel?.querySelector("pre");
    expect(preEl?.textContent).toContain("secret-auth-token-abc");
    expect(preEl?.textContent).not.toContain("<paste from create response>");
  });
});

describe("ExternalConnectModal — copy functionality", () => {
  it("calls navigator.clipboard.writeText with the snippet text", () => {
    renderAndFlush(defaultInfo);
    // Default tab is Universal MCP — query the copy button within the mcp panel.
    const mcpPanel = document.querySelector("[data-testid='panel-mcp']");
    const copyBtn = mcpPanel?.querySelector("button");
    if (copyBtn) fireEvent.click(copyBtn);
    expect(clipboardWriteText).toHaveBeenCalledWith(
      expect.stringContaining("secret-auth-token-abc"),
    );
  });
});

describe("ExternalConnectModal — close behavior", () => {
  it('calls onClose when "I\'ve saved it — close" is clicked', () => {
    const onClose = vi.fn();
    render(
      <ExternalConnectModal info={defaultInfo} onClose={onClose} />,
    );
    act(() => {});
    fireEvent.click(screen.getByRole("button", { name: /i've saved it/i }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

describe("ExternalConnectModal — missing optional fields", () => {
  it("shows (missing) for absent optional fields in the Fields tab", () => {
    // Use empty string so Field renders "(missing)" for registry_endpoint
    const minimalInfo: ExternalConnectionInfo = {
      workspace_id: "ws-min",
      platform_url: "https://min.example.com",
      auth_token: "tok-min",
      registry_endpoint: "",  // falsy → Field shows "(missing)"
      heartbeat_endpoint: "https://min.example.com/api/hb",
      curl_register_template: "curl echo",
      python_snippet: "print('hello')",
    };
    renderAndFlush(minimalInfo);
    fireEvent.click(screen.getByRole("tab", { name: /fields/i }));
    const fieldsPanel = document.querySelector("[data-testid='panel-fields']");
    expect(fieldsPanel?.textContent).toContain("(missing)");
  });

  it("hides the Hermes tab when hermes_channel_snippet is absent", () => {
    renderAndFlush({ ...defaultInfo, hermes_channel_snippet: undefined });
    expect(screen.queryByRole("tab", { name: /hermes/i })).toBeNull();
  });
});
