// @vitest-environment jsdom
/**
 * ServiceGroup — collapsible group of secret rows under a service header.
 *
 * Per spec §3.1:
 *   ── GitHub ────────────────────────── 1 key ──
 *   GITHUB_TOKEN
 *   ghp_••••••••••••••xK9f  [👁] [✓] [⎘] [✏] [🗑]
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs.
 *
 * Covers:
 *   - Renders group with role=group and aria-label
 *   - Service icon is aria-hidden
 *   - Label text matches service
 *   - Count: "1 key" for single, "N keys" for multiple
 *   - Renders SecretRow for each secret
 *   - Renders nothing when secrets array is empty (not called)
 *   - Different services show correct label and icon
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import React from "react";

import { ServiceGroup } from "../ServiceGroup";
import type { Secret, SecretGroup, ServiceConfig } from "@/types/secrets";

// ─── Mock SecretRow ────────────────────────────────────────────────────────────

vi.mock("../SecretRow", () => ({
  SecretRow: ({ secret, workspaceId }: { secret: Secret; workspaceId: string }) => (
    <div data-testid="secret-row" data-name={secret.name}>
      SecretRow:{secret.name}
    </div>
  ),
}));

// ─── Helpers ───────────────────────────────────────────────────────────────────

function makeService(icon: string, label: string): ServiceConfig {
  return { icon, label, docsUrl: "https://example.com/docs" };
}

function makeSecret(name: string): Secret {
  return {
    name,
    value: "sk-test-••••••••••••",
    group: "custom" as SecretGroup,
    masked: true,
  };
}

// ─── Tests ────────────────────────────────────────────────────────────────────

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

describe("ServiceGroup — render", () => {
  it("renders group with role=group", () => {
    const { container } = render(
      <ServiceGroup
        group="github"
        service={makeService("github", "GitHub")}
        secrets={[makeSecret("GITHUB_TOKEN")]}
        workspaceId="ws1"
      />,
    );
    expect(container.querySelector('[role="group"]')).toBeTruthy();
  });

  it("group aria-label contains service label", () => {
    const { container } = render(
      <ServiceGroup
        group="anthropic"
        service={makeService("anthropic", "Anthropic")}
        secrets={[makeSecret("ANTHROPIC_API_KEY")]}
        workspaceId="ws1"
      />,
    );
    const group = container.querySelector('[role="group"]');
    expect(group?.getAttribute("aria-label")).toContain("Anthropic");
  });

  it("service icon is aria-hidden", () => {
    const { container } = render(
      <ServiceGroup
        group="openrouter"
        service={makeService("openrouter", "OpenRouter")}
        secrets={[makeSecret("OPENROUTER_API_KEY")]}
        workspaceId="ws1"
      />,
    );
    const icon = container.querySelector('[aria-hidden="true"]');
    expect(icon).toBeTruthy();
    expect(icon?.textContent).toContain("🔀");
  });

  it("label text matches service label", () => {
    const { container } = render(
      <ServiceGroup
        group="github"
        service={makeService("github", "GitHub")}
        secrets={[makeSecret("GITHUB_TOKEN")]}
        workspaceId="ws1"
      />,
    );
    expect(container.textContent ?? "").toContain("GitHub");
  });

  it('count label is "1 key" for single secret', () => {
    const { container } = render(
      <ServiceGroup
        group="github"
        service={makeService("github", "GitHub")}
        secrets={[makeSecret("GITHUB_TOKEN")]}
        workspaceId="ws1"
      />,
    );
    expect(container.textContent ?? "").toContain("1 key");
  });

  it("count label is 'N keys' for multiple secrets", () => {
    const { container } = render(
      <ServiceGroup
        group="anthropic"
        service={makeService("anthropic", "Anthropic")}
        secrets={[
          makeSecret("ANTHROPIC_API_KEY"),
          makeSecret("ANTHROPIC_MODEL_PREF"),
        ]}
        workspaceId="ws1"
      />,
    );
    expect(container.textContent ?? "").toContain("2 keys");
  });

  it("renders SecretRow for each secret", () => {
    const { container } = render(
      <ServiceGroup
        group="github"
        service={makeService("github", "GitHub")}
        secrets={[
          makeSecret("GITHUB_TOKEN"),
          makeSecret("GITHUB_ORG"),
        ]}
        workspaceId="ws1"
      />,
    );
    const rows = container.querySelectorAll('[data-testid="secret-row"]');
    expect(rows).toHaveLength(2);
    expect(rows[0].getAttribute("data-name")).toBe("GITHUB_TOKEN");
    expect(rows[1].getAttribute("data-name")).toBe("GITHUB_ORG");
  });

  it("renders header and rows divs", () => {
    const { container } = render(
      <ServiceGroup
        group="github"
        service={makeService("github", "GitHub")}
        secrets={[makeSecret("GITHUB_TOKEN")]}
        workspaceId="ws1"
      />,
    );
    expect(container.querySelector(".service-group__header")).toBeTruthy();
    expect(container.querySelector(".service-group__rows")).toBeTruthy();
  });

  it("renders correct icon emoji for github", () => {
    const { container } = render(
      <ServiceGroup
        group="github"
        service={makeService("github", "GitHub")}
        secrets={[makeSecret("GITHUB_TOKEN")]}
        workspaceId="ws1"
      />,
    );
    const icon = container.querySelector(".service-group__icon");
    expect(icon?.textContent).toContain("🐙");
  });

  it("renders default icon for unknown service name", () => {
    const { container } = render(
      <ServiceGroup
        group="custom"
        service={makeService("unknown-service", "Custom Service")}
        secrets={[makeSecret("MY_CUSTOM_KEY")]}
        workspaceId="ws1"
      />,
    );
    const icon = container.querySelector(".service-group__icon");
    expect(icon?.textContent).toContain("🔑");
  });
});
