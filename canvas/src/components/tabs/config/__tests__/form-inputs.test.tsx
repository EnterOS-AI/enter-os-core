// @vitest-environment jsdom
/**
 * form-inputs — pure presentational form primitives for the Config tab.
 *
 * NOTE: No @testing-library/jest-dom import — use textContent / className /
 * getAttribute / checked / value checks to avoid "expect is not defined"
 * errors in this vitest configuration.
 *
 * Covers:
 *   - TextInput renders label and input with correct value
 *   - TextInput calls onChange with new value on keystroke
 *   - TextInput renders placeholder text when provided
 *   - TextInput applies mono class when mono=true
 *   - TextInput input has accessible aria-label from label
 *   - TextInput input is not mono by default
 *   - NumberInput renders label and number input
 *   - NumberInput calls onChange with parsed integer on keystroke
 *   - NumberInput calls onChange with 0 for non-numeric input
 *   - NumberInput respects min/max bounds
 *   - NumberInput input has aria-label from label prop
 *   - NumberInput input has font-mono class
 *   - Toggle renders checkbox with label text
 *   - Toggle renders checked/unchecked state correctly
 *   - Toggle calls onChange with boolean on toggle
 *   - TagList renders existing tags with remove buttons
 *   - TagList × button has aria-label "Remove tag {value}"
 *   - TagList calls onChange without removed tag on × click
 *   - TagList renders the label text
 *   - TagList renders placeholder text when provided
 *   - TagList renders exactly one textbox
 *   - TagList adds tag on Enter key
 *   - TagList does not add empty/whitespace-only tags on Enter
 *   - TagList clears input after adding tag
 *   - Section renders the title
 *   - Section renders children when open (defaultOpen=true)
 *   - Section starts closed when defaultOpen=false
 *   - Section opens/closes content on title click
 *   - Section button has aria-expanded reflecting open state
 *   - Section toggle indicator changes on open/close
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";

import {
  TextInput,
  NumberInput,
  Toggle,
  TagList,
  Section,
} from "../form-inputs";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

// ─── TextInput ───────────────────────────────────────────────────────────────

describe("TextInput", () => {
  it("renders the label text", () => {
    const { container } = render(
      <TextInput label="Agent Name" value="" onChange={vi.fn()} />,
    );
    expect(container.textContent).toContain("Agent Name");
  });

  it("renders the input with the given value", () => {
    render(<TextInput label="Model" value="claude-opus-4" onChange={vi.fn()} />);
    const input = document.querySelector("input") as HTMLInputElement;
    expect(input.value).toBe("claude-opus-4");
  });

  it("calls onChange with new value on keystroke", () => {
    const onChange = vi.fn();
    render(<TextInput label="Name" value="hello" onChange={onChange} />);
    const input = document.querySelector("input") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "hello world" } });
    expect(onChange).toHaveBeenCalledWith("hello world");
  });

  it("renders placeholder text when provided", () => {
    render(
      <TextInput
        label="Token"
        value=""
        onChange={vi.fn()}
        placeholder="sk-..."
      />,
    );
    const input = document.querySelector("input") as HTMLInputElement;
    expect(input.getAttribute("placeholder")).toBe("sk-...");
  });

  it("applies mono class when mono=true", () => {
    const { container } = render(
      <TextInput label="Model" value="" onChange={vi.fn()} mono />,
    );
    const input = container.querySelector("input") as HTMLInputElement;
    expect(input.className).toContain("font-mono");
  });

  it("input has aria-label matching the label", () => {
    render(<TextInput label="API Key" value="" onChange={vi.fn()} />);
    const input = document.querySelector("input") as HTMLInputElement;
    expect(input.getAttribute("aria-label")).toBe("API Key");
  });

  it("input is not mono by default", () => {
    const { container } = render(
      <TextInput label="Description" value="" onChange={vi.fn()} />,
    );
    const input = container.querySelector("input") as HTMLInputElement;
    expect(input.className).not.toContain("font-mono");
  });
});

// ─── NumberInput ─────────────────────────────────────────────────────────────

describe("NumberInput", () => {
  it("renders the label text", () => {
    const { container } = render(
      <NumberInput label="Timeout (s)" value={30} onChange={vi.fn()} />,
    );
    expect(container.textContent).toContain("Timeout (s)");
  });

  it("renders the input with the given numeric value", () => {
    render(<NumberInput label="Retries" value={3} onChange={vi.fn()} />);
    const input = document.querySelector("input[type=number]") as HTMLInputElement;
    expect(input.value).toBe("3");
  });

  it("calls onChange with parsed integer on keystroke", () => {
    const onChange = vi.fn();
    render(<NumberInput label="Delay" value={1} onChange={onChange} />);
    const input = document.querySelector("input[type=number]") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "7" } });
    expect(onChange).toHaveBeenCalledWith(7);
  });

  it("calls onChange with 0 for non-numeric input", () => {
    const onChange = vi.fn();
    render(<NumberInput label="Count" value={5} onChange={onChange} />);
    const input = document.querySelector("input[type=number]") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "abc" } });
    expect(onChange).toHaveBeenCalledWith(0);
  });

  it("respects min attribute", () => {
    render(
      <NumberInput
        label="Port"
        value={8000}
        onChange={vi.fn()}
        min={1024}
      />,
    );
    const input = document.querySelector("input[type=number]") as HTMLInputElement;
    expect(input.getAttribute("min")).toBe("1024");
  });

  it("respects max attribute", () => {
    render(
      <NumberInput
        label="Memory (MB)"
        value={256}
        onChange={vi.fn()}
        max={65535}
      />,
    );
    const input = document.querySelector("input[type=number]") as HTMLInputElement;
    expect(input.getAttribute("max")).toBe("65535");
  });

  it("input has aria-label from label prop", () => {
    render(<NumberInput label="Timeout" value={60} onChange={vi.fn()} />);
    const input = document.querySelector("input[type=number]") as HTMLInputElement;
    expect(input.getAttribute("aria-label")).toBe("Timeout");
  });

  it("input has font-mono class", () => {
    const { container } = render(
      <NumberInput label="Budget" value={100} onChange={vi.fn()} />,
    );
    const input = container.querySelector("input") as HTMLInputElement;
    expect(input.className).toContain("font-mono");
  });
});

// ─── Toggle ──────────────────────────────────────────────────────────────────

describe("Toggle", () => {
  it("renders the checkbox with label text", () => {
    const { container } = render(
      <Toggle label="Enable streaming" checked={false} onChange={vi.fn()} />,
    );
    const checkbox = container.querySelector(
      "input[type=checkbox]",
    ) as HTMLInputElement;
    expect(checkbox.checked).toBe(false);
    expect(
      checkbox.closest("label")?.textContent,
    ).toContain("Enable streaming");
  });

  it("renders checked state correctly", () => {
    const { container } = render(
      <Toggle label="Push notifications" checked onChange={vi.fn()} />,
    );
    const checkbox = container.querySelector(
      "input[type=checkbox]",
    ) as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
  });

  it("calls onChange with true when toggled on", () => {
    const onChange = vi.fn();
    const { container } = render(
      <Toggle label="Escalate" checked={false} onChange={onChange} />,
    );
    const checkbox = container.querySelector(
      "input[type=checkbox]",
    ) as HTMLInputElement;
    checkbox.click();
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("calls onChange with false when toggled off", () => {
    const onChange = vi.fn();
    const { container } = render(
      <Toggle label="Escalate" checked onChange={onChange} />,
    );
    const checkbox = container.querySelector(
      "input[type=checkbox]",
    ) as HTMLInputElement;
    checkbox.click();
    expect(onChange).toHaveBeenCalledWith(false);
  });

  it("checkbox is a native input element", () => {
    const { container } = render(
      <Toggle label="Feature flag" checked={false} onChange={vi.fn()} />,
    );
    expect(container.querySelector("input[type=checkbox]")).toBeTruthy();
  });
});

// ─── TagList ────────────────────────────────────────────────────────────────

describe("TagList", () => {
  it("renders existing tags", () => {
    const { container } = render(
      <TagList label="Tools" values={["file_read", "bash"]} onChange={vi.fn()} />,
    );
    expect(container.textContent).toContain("file_read");
    expect(container.textContent).toContain("bash");
  });

  it("renders × remove button for each tag with aria-label", () => {
    render(
      <TagList
        label="Skills"
        values={["python", "golang"]}
        onChange={vi.fn()}
      />,
    );
    const buttons = document.querySelectorAll("button");
    // buttons[0] = first × (python), buttons[1] = second × (golang)
    expect(buttons[0].getAttribute("aria-label")).toBe(
      "Remove tag python",
    );
    expect(buttons[1].getAttribute("aria-label")).toBe(
      "Remove tag golang",
    );
  });

  it("calls onChange without removed tag when × is clicked", () => {
    const onChange = vi.fn();
    render(
      <TagList
        label="Tags"
        values={["react", "vue", "angular"]}
        onChange={onChange}
      />,
    );
    const buttons = document.querySelectorAll("button");
    // buttons[0] = react ×, buttons[1] = vue ×, buttons[2] = angular ×
    buttons[0].click(); // Remove react
    expect(onChange).toHaveBeenCalledWith(["vue", "angular"]);
  });

  it("renders the label text", () => {
    const { container } = render(
      <TagList label="Required env vars" values={[]} onChange={vi.fn()} />,
    );
    expect(container.textContent).toContain("Required env vars");
  });

  it("renders placeholder text when provided", () => {
    render(
      <TagList
        label="Tags"
        values={[]}
        onChange={vi.fn()}
        placeholder="Add a tag..."
      />,
    );
    const input = document.querySelector("input[type=text]") as HTMLInputElement;
    expect(input.getAttribute("placeholder")).toBe("Add a tag...");
  });

  it("renders exactly one textbox (the input)", () => {
    const { container } = render(
      <TagList
        label="Tools"
        values={["read", "write"]}
        onChange={vi.fn()}
      />,
    );
    expect(
      container.querySelectorAll("input[type=text]"),
    ).toHaveLength(1);
  });

  it("adds tag on Enter key", () => {
    const onChange = vi.fn();
    render(
      <TagList label="Skills" values={["python"]} onChange={onChange} />,
    );
    const input = document.querySelector("input[type=text]") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "rust" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith(["python", "rust"]);
  });

  it("does not add empty tag on Enter", () => {
    const onChange = vi.fn();
    render(
      <TagList label="Tools" values={[]} onChange={onChange} />,
    );
    const input = document.querySelector("input[type=text]") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "   " } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onChange).not.toHaveBeenCalled();
  });

  it("clears input after adding tag", () => {
    render(
      <TagList label="Tags" values={[]} onChange={vi.fn()} />,
    );
    const input = document.querySelector("input[type=text]") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "golang" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(input.value).toBe("");
  });
});

// ─── Section ───────────────────────────────────────────────────────────────

describe("Section", () => {
  it("renders the title", () => {
    const { container } = render(
      <Section title="Runtime config">Content here</Section>,
    );
    expect(container.textContent).toContain("Runtime config");
  });

  it("renders children when open (defaultOpen=true)", () => {
    const { container } = render(
      <Section title="A section">Hidden content</Section>,
    );
    expect(container.textContent).toContain("Hidden content");
  });

  it("starts closed when defaultOpen=false", () => {
    const { container } = render(
      <Section title="Collapsed" defaultOpen={false}>
        Should not be visible
      </Section>,
    );
    expect(container.textContent).not.toContain("Should not be visible");
  });

  it("opens/closes content on title click", () => {
    const { container } = render(
      <Section title="Toggle me" defaultOpen={false}>
        Now you see me
      </Section>,
    );
    // Should be closed initially
    expect(container.textContent).not.toContain("Now you see me");
    // Click to open
    const btn = container.querySelector("button") as HTMLButtonElement;
    fireEvent.click(btn);
    expect(container.textContent).toContain("Now you see me");
    // Click to close
    fireEvent.click(btn);
    expect(container.textContent).not.toContain("Now you see me");
  });

  it("title button has aria-expanded reflecting open state", () => {
    // Open section
    const { container: openContainer } = render(
      <Section title="A section" defaultOpen={true}>
        Open content
      </Section>,
    );
    const openBtn = openContainer.querySelector(
      "button",
    ) as HTMLButtonElement;
    expect(openBtn.getAttribute("aria-expanded")).toBe("true");

    // Closed section
    const { container: closedContainer } = render(
      <Section title="B section" defaultOpen={false}>
        Closed content
      </Section>,
    );
    const closedBtn = closedContainer.querySelector(
      "button",
    ) as HTMLButtonElement;
    expect(closedBtn.getAttribute("aria-expanded")).toBe("false");
  });

  it("toggle indicator changes between ▾ (open) and ▸ (closed)", () => {
    // Open: uses ▾
    const { container: openContainer } = render(
      <Section title="Indicator" defaultOpen={true}>
        Open
      </Section>,
    );
    // Button has two spans: title (first) and indicator (second, aria-hidden)
    const openSpans = openContainer
      .querySelectorAll("button span");
    const openIndicator = openSpans[1]?.textContent?.trim();
    expect(openIndicator).toBe("▾");

    // Closed: uses ▸
    const { container: closedContainer } = render(
      <Section title="Indicator" defaultOpen={false}>
        Closed
      </Section>,
    );
    const closedSpans = closedContainer
      .querySelectorAll("button span");
    const closedIndicator = closedSpans[1]?.textContent?.trim();
    expect(closedIndicator).toBe("▸");
  });
});
