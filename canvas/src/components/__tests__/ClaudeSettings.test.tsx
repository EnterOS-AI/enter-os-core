// @vitest-environment jsdom
/**
 * Tests for issue #608 — effort + task_budget fields in workspace config.
 *
 * Covers:
 *   1. toYaml serialization (effort + task_budget → YAML keys)
 *   2. parseYaml round-trip (YAML → ConfigData)
 *   3. DEFAULT_CONFIG shape (new fields present with zero/empty defaults)
 *   4. ConfigTab source assertions (section rendered conditionally)
 *   5. React rendering of the section for claude-code and claude model configs
 */
import React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";

// ── Module-level mocks ───────────────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), put: vi.fn(), patch: vi.fn(), post: vi.fn() },
}));

const mockCanvasState = {
  restartWorkspace: vi.fn(),
  updateNodeData: vi.fn(),
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: Record<string, unknown>) => unknown) =>
      selector(mockCanvasState as Record<string, unknown>)
    ),
    { getState: () => mockCanvasState }
  ),
}));

vi.mock("../tabs/config/secrets-section", () => ({
  SecretsSection: () => <div data-testid="secrets-stub" />,
}));

// ── Imports ──────────────────────────────────────────────────────────────────

import { toYaml, parseYaml } from "../tabs/config/yaml-utils";
import { DEFAULT_CONFIG, type ConfigData } from "../tabs/config/form-inputs";
import { ConfigTab } from "../tabs/ConfigTab";
import { api } from "@/lib/api";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// ── 1. toYaml serialization ──────────────────────────────────────────────────

describe("toYaml — effort field", () => {
  it("omits effort when empty string", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "" };
    expect(toYaml(cfg)).not.toContain("effort:");
  });

  it("omits effort when undefined", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: undefined };
    expect(toYaml(cfg)).not.toContain("effort:");
  });

  it("serializes effort: low", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "low" };
    const yaml = toYaml(cfg);
    expect(yaml).toContain("effort: low");
  });

  it("serializes effort: medium", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "medium" };
    expect(toYaml(cfg)).toContain("effort: medium");
  });

  it("serializes effort: high", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "high" };
    expect(toYaml(cfg)).toContain("effort: high");
  });

  it("serializes effort: xhigh", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "xhigh" };
    expect(toYaml(cfg)).toContain("effort: xhigh");
  });

  it("serializes effort: max", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "max" };
    expect(toYaml(cfg)).toContain("effort: max");
  });
});

describe("toYaml — task_budget field", () => {
  it("omits task_budget when 0", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, task_budget: 0 };
    expect(toYaml(cfg)).not.toContain("task_budget:");
  });

  it("omits task_budget when undefined", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, task_budget: undefined };
    expect(toYaml(cfg)).not.toContain("task_budget:");
  });

  it("serializes task_budget: 10000", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, task_budget: 10000 };
    expect(toYaml(cfg)).toContain("task_budget: 10000");
  });

  it("serializes task_budget: 50000", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, task_budget: 50000 };
    expect(toYaml(cfg)).toContain("task_budget: 50000");
  });
});

describe("toYaml — effort and task_budget together", () => {
  it("serializes both when set", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "xhigh", task_budget: 32000 };
    const yaml = toYaml(cfg);
    expect(yaml).toContain("effort: xhigh");
    expect(yaml).toContain("task_budget: 32000");
  });

  it("effort appears before task_budget in output", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "high", task_budget: 8000 };
    const yaml = toYaml(cfg);
    const effortIdx = yaml.indexOf("effort:");
    const budgetIdx = yaml.indexOf("task_budget:");
    expect(effortIdx).toBeGreaterThan(-1);
    expect(budgetIdx).toBeGreaterThan(-1);
    expect(effortIdx).toBeLessThan(budgetIdx);
  });
});

// ── 2. parseYaml round-trip ──────────────────────────────────────────────────

describe("parseYaml — effort + task_budget round-trip", () => {
  it("parses effort from YAML", () => {
    const yaml = "name: Test\neffort: high\n";
    const parsed = parseYaml(yaml);
    expect(parsed.effort).toBe("high");
  });

  it("parses task_budget from YAML as integer", () => {
    const yaml = "name: Test\ntask_budget: 16000\n";
    const parsed = parseYaml(yaml);
    expect(parsed.task_budget).toBe(16000);
  });

  it("round-trips effort: xhigh through toYaml → parseYaml", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "xhigh" };
    const yaml = toYaml(cfg);
    const parsed = parseYaml(yaml);
    expect(parsed.effort).toBe("xhigh");
  });

  it("round-trips task_budget: 50000 through toYaml → parseYaml", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, task_budget: 50000 };
    const yaml = toYaml(cfg);
    const parsed = parseYaml(yaml);
    expect(parsed.task_budget).toBe(50000);
  });

  it("round-trips both fields together", () => {
    const cfg: ConfigData = { ...DEFAULT_CONFIG, effort: "low", task_budget: 1000 };
    const yaml = toYaml(cfg);
    const parsed = parseYaml(yaml);
    expect(parsed.effort).toBe("low");
    expect(parsed.task_budget).toBe(1000);
  });
});

// ── 3. DEFAULT_CONFIG shape ──────────────────────────────────────────────────

describe("DEFAULT_CONFIG", () => {
  it("has effort defaulting to empty string", () => {
    expect(DEFAULT_CONFIG.effort).toBe("");
  });

  it("has task_budget defaulting to 0", () => {
    expect(DEFAULT_CONFIG.task_budget).toBe(0);
  });
});

// ── 4. ConfigTab source assertions ──────────────────────────────────────────

describe("ConfigTab source — Claude Settings section", () => {
  it("ConfigTab.tsx contains the effort-select data-testid", async () => {
    const { readFileSync } = await import("fs");
    const { join } = await import("path");
    const src = readFileSync(join(__dirname, "../../components/tabs/ConfigTab.tsx"), "utf8");
    expect(src).toContain('data-testid="effort-select"');
    expect(src).toContain('data-testid="task-budget-input"');
  });

  it("ConfigTab.tsx effort dropdown has all five Claude values", async () => {
    const { readFileSync } = await import("fs");
    const { join } = await import("path");
    const src = readFileSync(join(__dirname, "../../components/tabs/ConfigTab.tsx"), "utf8");
    expect(src).toContain('"low"');
    expect(src).toContain('"medium"');
    expect(src).toContain('"high"');
    expect(src).toContain('"xhigh"');
    expect(src).toContain('"max"');
  });

  it("ConfigTab.tsx section is guarded by claude-code runtime check", async () => {
    const { readFileSync } = await import("fs");
    const { join } = await import("path");
    const src = readFileSync(join(__dirname, "../../components/tabs/ConfigTab.tsx"), "utf8");
    expect(src).toContain('config.runtime === "claude-code"');
    expect(src).toContain('"claude"');
  });
});

// ── 5. React rendering ───────────────────────────────────────────────────────

describe("ConfigTab — Claude Settings section rendering", () => {
  function setupMock(configYaml: string) {
    vi.mocked(api.get).mockResolvedValue({ content: configYaml } as never);
  }

  it("shows Claude Settings section for claude-code runtime", async () => {
    setupMock("name: Bot\nruntime: claude-code\n");
    render(<ConfigTab workspaceId="ws-1" />);
    // Section title appears once loading resolves
    const section = await screen.findByText("Claude Settings");
    expect(section).toBeTruthy();
  });

  it("shows Claude Settings section when model contains claude", async () => {
    setupMock("name: Bot\nmodel: anthropic:claude-opus-4-7\n");
    render(<ConfigTab workspaceId="ws-1" />);
    const section = await screen.findByText("Claude Settings");
    expect(section).toBeTruthy();
  });

  it("does NOT show Claude Settings section for non-claude runtime/model", async () => {
    setupMock("name: Bot\nruntime: crewai\nmodel: openai:gpt-4o\n");
    render(<ConfigTab workspaceId="ws-1" />);
    // Wait for load (config.yaml fetch resolves) then check absence
    await screen.findByText("General"); // loaded
    expect(screen.queryByText("Claude Settings")).toBeNull();
  });
});
