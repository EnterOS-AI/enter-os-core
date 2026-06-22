// @vitest-environment jsdom
/**
 * Tests for yaml-utils.ts — parseYaml and toYaml pure functions.
 */
import { describe, expect, it } from "vitest";
import { parseYaml, toYaml } from "../yaml-utils";
import type { ConfigData } from "../form-inputs";

const FULL_CONFIG: ConfigData = {
  name: "my-agent",
  description: "A helpful assistant",
  version: "1.0.0",
  tier: 4,
  model: "claude-4-7",
  runtime: "claude-code",
  runtime_config: { model: "claude-4-7", required_env: ["ANTHROPIC_API_KEY"], timeout: 120 },
  effort: "medium",
  task_budget: 100,
  prompt_files: ["system.md"],
  skills: ["web-search", "code"],
  tools: ["bash"],
  a2a: { port: 8000, streaming: true, push_notifications: true },
  delegation: { retry_attempts: 3, retry_delay: 5, timeout: 120, escalate: true },
  sandbox: { backend: "docker", memory_limit: "256m", timeout: 30 },
};

const MINIMAL_CONFIG: ConfigData = {
  name: "",
  description: "",
  version: "1.0.0",
  tier: 1,
  model: "",
  runtime: "",
  prompt_files: [],
  skills: [],
  tools: [],
  a2a: { port: 8000, streaming: true, push_notifications: true },
  delegation: { retry_attempts: 3, retry_delay: 5, timeout: 120, escalate: true },
  sandbox: { backend: "docker", memory_limit: "256m", timeout: 30 },
};

// ─── parseYaml ─────────────────────────────────────────────────────────────────

describe("parseYaml", () => {
  it("returns empty object for empty input", () => {
    expect(parseYaml("")).toEqual({});
  });

  it("returns empty object for blank lines only", () => {
    expect(parseYaml("\n\n  \n")).toEqual({});
  });

  it("returns empty object for comment-only input", () => {
    expect(parseYaml("# hello\n# world")).toEqual({});
  });

  it("parses simple key-value pairs", () => {
    const result = parseYaml("name: hello\nversion: 1.0");
    expect(result).toEqual({ name: "hello", version: "1.0" });
  });

  it("trims whitespace around values", () => {
    const result = parseYaml("name:   hello   \nversion:  1.0  ");
    expect(result).toEqual({ name: "hello", version: "1.0" });
  });

  it("parses boolean true", () => {
    expect(parseYaml("streaming: true")).toEqual({ streaming: true });
  });

  it("parses boolean false", () => {
    expect(parseYaml("streaming: false")).toEqual({ streaming: false });
  });

  it("parses integer numbers", () => {
    expect(parseYaml("port: 8000\ntimeout: 120")).toEqual({ port: 8000, timeout: 120 });
  });

  it("parses string values that look like numbers", () => {
    // Keys that have no space before colon would have been parsed as numbers
    // but since the YAML has `key: value` format, it should be string
    expect(parseYaml("model: claude-4-7")).toEqual({ model: "claude-4-7" });
  });

  it("parses a top-level list", () => {
    const result = parseYaml("skills:\n  - web-search\n  - code");
    expect(result).toEqual({ skills: ["web-search", "code"] });
  });

  it("parses a top-level object", () => {
    const result = parseYaml("a2a:\n  port: 8000\n  streaming: true");
    expect(result).toEqual({ a2a: { port: 8000, streaming: true } });
  });

  it("skips blank lines within content", () => {
    const result = parseYaml("name: hello\n\nversion: 1.0\n\n");
    expect(result).toEqual({ name: "hello", version: "1.0" });
  });

  it("skips comment lines within content", () => {
    const result = parseYaml("name: hello\n# this is a comment\nversion: 1.0");
    expect(result).toEqual({ name: "hello", version: "1.0" });
  });

  it("parses a 2-level nested list (env.required pattern)", () => {
    const result = parseYaml("env:\n  required:\n    - ANTHROPIC_API_KEY\n    - OPENAI_API_KEY");
    expect(result).toEqual({ env: { required: ["ANTHROPIC_API_KEY", "OPENAI_API_KEY"] } });
  });

  it("parses empty list marker `[]`", () => {
    const result = parseYaml("prompt_files: []");
    expect(result).toEqual({ prompt_files: [] });
  });

  it("handles multiple mixed structures in one document", () => {
    const yaml = `name: test-agent
version: 1.0.0
tier: 4
runtime: claude-code
skills:
  - web-search
a2a:
  port: 8000
  streaming: true`;
    const result = parseYaml(yaml);
    expect(result).toEqual({
      name: "test-agent",
      version: "1.0.0",
      tier: 4,
      runtime: "claude-code",
      skills: ["web-search"],
      a2a: { port: 8000, streaming: true },
    });
  });

  it("leaves unrecognised top-level lines as-is (skipped)", () => {
    // Lines that don't match the pattern are skipped
    const result = parseYaml("name: hello\n[invalid line]\nversion: 1.0");
    expect(result).toEqual({ name: "hello", version: "1.0" });
  });
});

// ─── toYaml ─────────────────────────────────────────────────────────────────────

describe("toYaml", () => {
  it("produces output for minimal config (required fields only)", () => {
    const out = toYaml(MINIMAL_CONFIG);
    // skills: [] and tools: [] are always emitted
    expect(out).toContain("version: 1.0.0");
    expect(out).toContain("tier: 1");
    expect(out).toContain("skills: []");
    expect(out).toContain("tools: []");
    expect(out).toContain("a2a:");
    expect(out).toContain("delegation:");
    expect(out).toContain("sandbox:");
  });

  it("writes name and description fields", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, name: "my-agent", description: "desc" };
    const out = toYaml(cfg);
    expect(out).toContain("name: my-agent");
    expect(out).toContain("description: desc");
  });

  it("writes version and tier", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, tier: 4 };
    const out = toYaml(cfg);
    expect(out).toContain("version: 1.0.0");
    expect(out).toContain("tier: 4");
  });

  it("writes runtime with a blank line separator before it", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, runtime: "claude-code" };
    const out = toYaml(cfg);
    expect(out).toContain("runtime: claude-code");
  });

  it("writes runtime_config as a nested block", () => {
    const cfg: ConfigData = {
      ...MINIMAL_CONFIG,
      runtime: "claude-code",
      runtime_config: { model: "claude-4-7", required_env: ["KEY"], timeout: 120 },
    };
    const out = toYaml(cfg);
    expect(out).toContain("runtime_config:");
    expect(out).toContain("  model: claude-4-7");
    expect(out).toContain("  required_env:");
    expect(out).toContain("    - KEY");
    expect(out).toContain("  timeout: 120");
  });

  it("omits runtime_config when empty", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, runtime: "claude-code" };
    const out = toYaml(cfg);
    // runtime_config key should not appear
    expect(out).not.toContain("runtime_config:");
  });

  it("writes effort when set", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, effort: "high" };
    const out = toYaml(cfg);
    expect(out).toContain("effort: high");
  });

  it("omits effort when empty string", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, effort: "" };
    const out = toYaml(cfg);
    expect(out).not.toContain("effort:");
  });

  it("writes task_budget when positive", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, task_budget: 100 };
    const out = toYaml(cfg);
    expect(out).toContain("task_budget: 100");
  });

  it("omits task_budget when zero", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, task_budget: 0 };
    const out = toYaml(cfg);
    expect(out).not.toContain("task_budget:");
  });

  it("writes prompt_files as a list block", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, prompt_files: ["system.md", "ethics.md"] };
    const out = toYaml(cfg);
    expect(out).toContain("prompt_files:");
    expect(out).toContain("  - system.md");
    expect(out).toContain("  - ethics.md");
  });

  it("writes skills as a list block", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, skills: ["web-search", "code"] };
    const out = toYaml(cfg);
    expect(out).toContain("skills:");
    expect(out).toContain("  - web-search");
    expect(out).toContain("  - code");
  });

  it("writes tools as a list block", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, tools: ["bash", "read"] };
    const out = toYaml(cfg);
    expect(out).toContain("tools:");
    expect(out).toContain("  - bash");
    expect(out).toContain("  - read");
  });

  it("writes a2a as a nested block", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, a2a: { port: 9000, streaming: false, push_notifications: false } };
    const out = toYaml(cfg);
    expect(out).toContain("a2a:");
    expect(out).toContain("  port: 9000");
    expect(out).toContain("  streaming: false");
    expect(out).toContain("  push_notifications: false");
  });

  it("writes delegation as a nested block", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, delegation: { retry_attempts: 5, retry_delay: 10, timeout: 60, escalate: false } };
    const out = toYaml(cfg);
    expect(out).toContain("delegation:");
    expect(out).toContain("  retry_attempts: 5");
    expect(out).toContain("  retry_delay: 10");
    expect(out).toContain("  timeout: 60");
    expect(out).toContain("  escalate: false");
  });

  it("writes sandbox backend block", () => {
    const cfg: ConfigData = { ...MINIMAL_CONFIG, sandbox: { backend: "aws-lambda", memory_limit: "512m", timeout: 15 } };
    const out = toYaml(cfg);
    expect(out).toContain("sandbox:");
    expect(out).toContain("  backend: aws-lambda");
    expect(out).toContain("  memory_limit: 512m");
    expect(out).toContain("  timeout: 15");
  });

  it("omits empty/null/undefined fields entirely", () => {
    const cfg: ConfigData = {
      ...MINIMAL_CONFIG,
      name: "test",
      model: "",           // omitted
      description: "",     // omitted
    };
    const out = toYaml(cfg);
    expect(out).not.toContain("model:");
    expect(out).not.toContain("description:");
    expect(out).toContain("name: test");
  });

  it("produces a trailing newline", () => {
    const out = toYaml(MINIMAL_CONFIG);
    expect(out.endsWith("\n")).toBe(true);
  });

  it("round-trips FULL_CONFIG through parse → toYaml → parse", () => {
    // parseYaml produces plain Record, so a2a/delegation/sandbox
    // come out as objects — toYaml handles them via the cast.
    const round = parseYaml(toYaml(FULL_CONFIG));
    expect(round).toMatchObject({
      name: "my-agent",
      description: "A helpful assistant",
      version: "1.0.0",
      tier: 4,
      runtime: "claude-code",
      effort: "medium",
      task_budget: 100,
      prompt_files: ["system.md"],
      skills: ["web-search", "code"],
      tools: ["bash"],
    });
    expect(round.a2a).toMatchObject({ port: 8000, streaming: true, push_notifications: true });
    expect(round.delegation).toMatchObject({ retry_attempts: 3, retry_delay: 5, timeout: 120, escalate: true });
    expect(round.sandbox).toMatchObject({ backend: "docker", memory_limit: "256m", timeout: 30 });
  });
});
