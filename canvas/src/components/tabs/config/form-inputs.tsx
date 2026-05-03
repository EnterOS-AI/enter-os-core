"use client";

import { useState } from "react";

export interface ConfigData {
  name: string;
  description: string;
  version: string;
  tier: number;
  model: string;
  runtime: string;
  runtime_config?: {
    model?: string;
    required_env?: string[];
    timeout?: number;
    // Deprecated
    auth_token_file?: string;
  };
  // Claude API primitives (Opus 4.7+) — issue #608
  // effort maps to output_config.effort in Messages API: 'low' | 'medium' | 'high' | 'xhigh'
  effort?: string;
  // task_budget maps to output_config.task_budget.total (requires beta header task-budgets-2026-03-13)
  task_budget?: number;
  prompt_files: string[];
  shared_context: string[];
  skills: string[];
  tools: string[];
  a2a: { port: number; streaming: boolean; push_notifications: boolean };
  delegation: { retry_attempts: number; retry_delay: number; timeout: number; escalate: boolean };
  sandbox: { backend: string; memory_limit: string; timeout: number };
}

export const DEFAULT_CONFIG: ConfigData = {
  name: "",
  description: "",
  version: "1.0.0",
  tier: 1,
  model: "",
  runtime: "",
  effort: "",
  task_budget: 0,
  prompt_files: [],
  shared_context: [],
  skills: [],
  tools: [],
  a2a: { port: 8000, streaming: true, push_notifications: true },
  delegation: { retry_attempts: 3, retry_delay: 5, timeout: 120, escalate: true },
  sandbox: { backend: "docker", memory_limit: "256m", timeout: 30 },
};

export function TextInput({ label, value, onChange, placeholder, mono }: { label: string; value: string; onChange: (v: string) => void; placeholder?: string; mono?: boolean }) {
  const id = `textinput-${label.toLowerCase().replace(/\s+/g, "-")}`;
  return (
    <div>
      <label htmlFor={id} className="text-[10px] text-ink-soft block mb-1">{label}</label>
      <input
        id={id}
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={label}
        className={`w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent ${mono ? "font-mono" : ""}`}
      />
    </div>
  );
}

export function NumberInput({ label, value, onChange, min, max }: { label: string; value: number; onChange: (v: number) => void; min?: number; max?: number }) {
  const id = `numberinput-${label.toLowerCase().replace(/\s+/g, "-")}`;
  return (
    <div>
      <label htmlFor={id} className="text-[10px] text-ink-soft block mb-1">{label}</label>
      <input
        id={id}
        type="number"
        value={value}
        onChange={(e) => onChange(parseInt(e.target.value, 10) || 0)}
        min={min}
        max={max}
        aria-label={label}
        className="w-full bg-surface-card border border-line rounded px-2 py-1 text-xs text-ink focus:outline-none focus:border-accent font-mono"
      />
    </div>
  );
}

export function Toggle({ label, checked, onChange }: { label: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <label className="flex items-center gap-2 cursor-pointer">
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} className="accent-blue-500" />
      <span className="text-[10px] text-ink-mid">{label}</span>
    </label>
  );
}

export function TagList({ label, values, onChange, placeholder }: { label: string; values: string[]; onChange: (v: string[]) => void; placeholder?: string }) {
  const id = `taglist-${label.toLowerCase().replace(/\s+/g, "-")}`;
  const [input, setInput] = useState("");
  return (
    <div>
      <label htmlFor={id} className="text-[10px] text-ink-soft block mb-1">{label}</label>
      <div className="flex flex-wrap gap-1 mb-1">
        {values.map((v, i) => (
          <span key={i} className="inline-flex items-center gap-1 px-1.5 py-0.5 bg-surface-card border border-line rounded text-[10px] text-ink-mid font-mono">
            {v}
            <button type="button" aria-label={`Remove tag ${v}`} onClick={() => onChange(values.filter((_, j) => j !== i))} className="text-ink-soft hover:text-bad">×</button>
          </span>
        ))}
      </div>
      <input
        id={id}
        type="text"
        value={input}
        onChange={(e) => setInput(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && input.trim()) {
            onChange([...values, input.trim()]);
            setInput("");
          }
        }}
        placeholder={placeholder || "Type and press Enter"}
        aria-label={label}
        className="w-full bg-surface-card border border-line rounded px-2 py-1 text-[10px] text-ink focus:outline-none focus:border-accent font-mono"
      />
    </div>
  );
}

export function Section({ title, children, defaultOpen = true }: { title: string; children: React.ReactNode; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <div className="border border-line rounded mb-2">
      <button type="button" onClick={() => setOpen(!open)} className="w-full flex items-center justify-between px-3 py-1.5 text-[10px] text-ink-mid hover:text-ink bg-surface-sunken/50">
        <span className="font-medium uppercase tracking-wider">{title}</span>
        <span>{open ? "▾" : "▸"}</span>
      </button>
      {open && <div className="p-3 space-y-3">{children}</div>}
    </div>
  );
}
