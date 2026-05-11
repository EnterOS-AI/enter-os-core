// @vitest-environment jsdom
"use client";
/**
 * Tests for form-inputs.tsx — 35 cases:
 * TextInput (7), NumberInput (8), Toggle (5), TagList (9), Section (6).
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import React from "react";

import {
  TextInput,
  NumberInput,
  Toggle,
  TagList,
  Section,
} from "../form-inputs";

afterEach(cleanup);

// ─── TextInput ───────────────────────────────────────────────────────────────

describe("TextInput", () => {
  describe("renders", () => {
    it("renders the label", () => {
      render(<TextInput label="API Key" value="" onChange={vi.fn()} />);
      expect(screen.getByLabelText("API Key")).toBeTruthy();
    });

    it("renders the current value", () => {
      render(<TextInput label="Name" value="Claude" onChange={vi.fn()} />);
      expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("Claude");
    });

    it("calls onChange when value changes", () => {
      const onChange = vi.fn();
      render(<TextInput label="Name" value="" onChange={onChange} />);
      fireEvent.change(screen.getByRole("textbox"), { target: { value: "Sonnet" } });
      expect(onChange).toHaveBeenCalledWith("Sonnet");
    });

    it("renders placeholder when provided", () => {
      render(<TextInput label="Name" value="" onChange={vi.fn()} placeholder="Enter your name" />);
      expect((screen.getByRole("textbox") as HTMLInputElement).placeholder).toBe("Enter your name");
    });

    it("applies font-mono class when mono=true", () => {
      render(<TextInput label="Token" value="" onChange={vi.fn()} mono />);
      const input = screen.getByRole("textbox");
      expect(input.className).toMatch(/font-mono/);
    });

    it("has aria-label matching the label", () => {
      render(<TextInput label="API Key" value="" onChange={vi.fn()} />);
      expect(screen.getByRole("textbox").getAttribute("aria-label")).toBe("API Key");
    });

    it("does not apply font-mono class when mono=false", () => {
      render(<TextInput label="Name" value="" onChange={vi.fn()} mono={false} />);
      expect(screen.getByRole("textbox").className).not.toMatch(/font-mono/);
    });
  });
});

// ─── NumberInput ────────────────────────────────────────────────────────────

describe("NumberInput", () => {
  describe("renders", () => {
    it("renders the label", () => {
      render(<NumberInput label="Port" value={8000} onChange={vi.fn()} />);
      expect(screen.getByLabelText("Port")).toBeTruthy();
    });

    it("renders the numeric value", () => {
      render(<NumberInput label="Timeout" value={120} onChange={vi.fn()} />);
      expect((screen.getByRole("spinbutton") as HTMLInputElement).value).toBe("120");
    });

    it("calls onChange with parsed integer", () => {
      const onChange = vi.fn();
      render(<NumberInput label="Retries" value={0} onChange={onChange} />);
      fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "3" } });
      expect(onChange).toHaveBeenCalledWith(3);
    });

    it("calls onChange with 0 for non-numeric input", () => {
      const onChange = vi.fn();
      render(<NumberInput label="Retries" value={0} onChange={onChange} />);
      fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "abc" } });
      expect(onChange).toHaveBeenCalledWith(0);
    });

    it("applies min/max attributes", () => {
      render(<NumberInput label="Priority" value={5} onChange={vi.fn()} min={1} max={10} />);
      const input = screen.getByRole("spinbutton") as HTMLInputElement;
      expect(input.min).toBe("1");
      expect(input.max).toBe("10");
    });

    it("has aria-label matching the label", () => {
      render(<NumberInput label="Retries" value={3} onChange={vi.fn()} />);
      expect(screen.getByRole("spinbutton").getAttribute("aria-label")).toBe("Retries");
    });

    it("applies font-mono class", () => {
      render(<NumberInput label="Timeout" value={30} onChange={vi.fn()} />);
      expect(screen.getByRole("spinbutton").className).toMatch(/font-mono/);
    });
  });
});

// ─── Toggle ─────────────────────────────────────────────────────────────────

describe("Toggle", () => {
  describe("renders", () => {
    it("renders a checkbox", () => {
      render(<Toggle label="Enable streaming" checked={false} onChange={vi.fn()} />);
      expect(screen.getByRole("checkbox")).toBeTruthy();
    });

    it("reflects checked=true state", () => {
      render(<Toggle label="Enable streaming" checked={true} onChange={vi.fn()} />);
      expect((screen.getByRole("checkbox") as HTMLInputElement).checked).toBe(true);
    });

    it("reflects checked=false state", () => {
      render(<Toggle label="Enable streaming" checked={false} onChange={vi.fn()} />);
      expect((screen.getByRole("checkbox") as HTMLInputElement).checked).toBe(false);
    });

    it("calls onChange with new boolean value", () => {
      const onChange = vi.fn();
      render(<Toggle label="Enable streaming" checked={false} onChange={onChange} />);
      fireEvent.click(screen.getByRole("checkbox"));
      expect(onChange).toHaveBeenCalledWith(true);
    });

    it("renders as type=checkbox", () => {
      render(<Toggle label="Enable" checked={false} onChange={vi.fn()} />);
      expect(screen.getByRole("checkbox").getAttribute("type")).toBe("checkbox");
    });
  });
});

// ─── TagList ───────────────────────────────────────────────────────────────

describe("TagList", () => {
  describe("renders", () => {
    it("renders existing tags", () => {
      render(<TagList label="Skills" values={["python", "go"]} onChange={vi.fn()} />);
      expect(screen.getByText("python")).toBeTruthy();
      expect(screen.getByText("go")).toBeTruthy();
    });

    it("calls onChange with updated array when × clicked", () => {
      const onChange = vi.fn();
      render(<TagList label="Skills" values={["python", "go"]} onChange={onChange} />);
      fireEvent.click(screen.getByRole("button", { name: /remove tag python/i }));
      expect(onChange).toHaveBeenCalledWith(["go"]);
    });

    it("× button has correct aria-label per tag", () => {
      render(<TagList label="Skills" values={["python"]} onChange={vi.fn()} />);
      expect(screen.getByRole("button", { name: /remove tag python/i })).toBeTruthy();
    });

    it("adds tag when Enter is pressed with non-empty input", () => {
      const onChange = vi.fn();
      render(<TagList label="Skills" values={[]} onChange={onChange} />);
      const input = screen.getByRole("textbox");
      fireEvent.change(input, { target: { value: "rust" } });
      fireEvent.keyDown(input, { key: "Enter" });
      expect(onChange).toHaveBeenCalledWith(["rust"]);
    });

    it("does not add tag when Enter is pressed with whitespace-only input", () => {
      const onChange = vi.fn();
      render(<TagList label="Skills" values={[]} onChange={onChange} />);
      const input = screen.getByRole("textbox");
      fireEvent.change(input, { target: { value: "   " } });
      fireEvent.keyDown(input, { key: "Enter" });
      expect(onChange).not.toHaveBeenCalled();
    });

    it("clears input after adding a tag", () => {
      const onChange = vi.fn();
      render(<TagList label="Skills" values={[]} onChange={onChange} />);
      const input = screen.getByRole("textbox");
      fireEvent.change(input, { target: { value: "typescript" } });
      fireEvent.keyDown(input, { key: "Enter" });
      expect((input as HTMLInputElement).value).toBe("");
    });

    it("renders the label", () => {
      render(<TagList label="Tools" values={[]} onChange={vi.fn()} />);
      expect(screen.getByLabelText("Tools")).toBeTruthy();
    });

    it("renders placeholder text", () => {
      render(<TagList label="Skills" values={[]} onChange={vi.fn()} placeholder="Add a skill" />);
      expect((screen.getByRole("textbox") as HTMLInputElement).placeholder).toBe("Add a skill");
    });

    it("renders default placeholder when not specified", () => {
      render(<TagList label="Skills" values={[]} onChange={vi.fn()} />);
      expect((screen.getByRole("textbox") as HTMLInputElement).placeholder).toBe("Type and press Enter");
    });
  });
});

// ─── Section ────────────────────────────────────────────────────────────────

describe("Section", () => {
  describe("renders", () => {
    it("renders the title", () => {
      render(<Section title="Runtime Config"><p>Content</p></Section>);
      expect(screen.getByText("Runtime Config")).toBeTruthy();
    });

    it("renders children when defaultOpen=true", () => {
      render(<Section title="Runtime Config"><p data-testid="content">Hello</p></Section>);
      expect(screen.getByTestId("content")).toBeTruthy();
    });

    it("hides children when defaultOpen=false", () => {
      render(<Section title="Runtime Config" defaultOpen={false}><p data-testid="content">Hello</p></Section>);
      expect(screen.queryByTestId("content")).toBeNull();
    });

    it("toggles children visibility on click", () => {
      render(<Section title="Runtime Config" defaultOpen={true}><p data-testid="content">Hello</p></Section>);
      expect(screen.getByTestId("content")).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: /runtime config/i }));
      expect(screen.queryByTestId("content")).toBeNull();
    });

    it("button has aria-expanded reflecting open state", () => {
      render(<Section title="Runtime Config" defaultOpen={true}><p>Content</p></Section>);
      const btn = screen.getByRole("button", { name: /runtime config/i });
      expect(btn.getAttribute("aria-expanded")).toBe("true");
      fireEvent.click(btn);
      expect(btn.getAttribute("aria-expanded")).toBe("false");
    });

    it("button has aria-controls linking to content region id", () => {
      render(<Section title="Runtime Config"><p>Content</p></Section>);
      const btn = screen.getByRole("button", { name: /runtime config/i });
      const contentId = btn.getAttribute("aria-controls");
      expect(contentId).not.toBeNull();
      // Content div has the matching id
      expect(document.getElementById(String(contentId))).not.toBeNull();
    });

    it("indicator span has aria-hidden so screen readers skip it", () => {
      render(<Section title="Runtime Config"><p>Content</p></Section>);
      const btn = screen.getByRole("button", { name: /runtime config/i });
      const indicator = btn.querySelector("[aria-hidden='true']");
      expect(indicator).not.toBeNull();
    });
  });
});
