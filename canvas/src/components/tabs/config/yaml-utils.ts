import type { ConfigData } from "./form-inputs";

// Simple YAML parser for config.yaml — handles flat keys, 1-level objects,
// lists, and 2-level nesting (e.g., env.required: [...]).
export function parseYaml(text: string): Record<string, unknown> {
  const result: Record<string, unknown> = {};
  const lines = text.split("\n");

  function parseValue(v: string): unknown {
    if (v === "true") return true;
    if (v === "false") return false;
    if (/^\d+$/.test(v)) return parseInt(v, 10);
    return v;
  }

  let i = 0;
  while (i < lines.length) {
    const line = lines[i];

    // Skip blanks and comments
    if (line.trim() === "" || line.trim().startsWith("#")) { i++; continue; }

    // Top-level key
    const topMatch = line.match(/^(\w[\w_]*):\s*(.*)/);
    if (!topMatch) { i++; continue; }

    const key = topMatch[1];
    const val = topMatch[2].trim();
    i++;

    if (val !== "" && val !== "[]") {
      result[key] = parseValue(val);
      continue;
    }

    // Peek ahead to determine structure
    const nextLine = lines[i] || "";
    if (val === "[]" || (!nextLine.match(/^\s/) || nextLine.trim() === "" || nextLine.trim().startsWith("#"))) {
      result[key] = val === "[]" ? [] : "";
      continue;
    }

    // Indented content follows — is it a list or object?
    if (nextLine.match(/^\s+-\s+/)) {
      // List
      const items: string[] = [];
      while (i < lines.length && lines[i].match(/^\s+-\s+/)) {
        items.push(lines[i].replace(/^\s+-\s+/, "").trim());
        i++;
      }
      result[key] = items;
    } else if (nextLine.match(/^\s+\w+:/)) {
      // Object (1 or 2 levels)
      const obj: Record<string, unknown> = {};
      while (i < lines.length) {
        const sub = lines[i];
        if (sub.trim() === "" || sub.trim().startsWith("#")) { i++; continue; }
        // 2-space indented key: value
        const subMatch = sub.match(/^  (\w[\w_]*):\s*(.*)/);
        if (!subMatch) break;
        const subKey = subMatch[1];
        const subVal = subMatch[2].trim();
        i++;

        if (subVal !== "" && subVal !== "[]") {
          obj[subKey] = parseValue(subVal);
        } else {
          // Check for nested list (2-level: env.required: [...])
          const subNext = lines[i] || "";
          if (subNext.match(/^\s{4,}-\s+/)) {
            const subItems: string[] = [];
            while (i < lines.length && lines[i].match(/^\s{4,}-\s+/)) {
              subItems.push(lines[i].replace(/^\s+-\s+/, "").trim());
              i++;
            }
            obj[subKey] = subItems;
          } else {
            obj[subKey] = subVal === "[]" ? [] : "";
          }
        }
      }
      result[key] = obj;
    }
  }
  return result;
}

export function toYaml(config: ConfigData): string {
  const lines: string[] = [];
  const simple = (k: string, v: unknown) => {
    if (v === undefined || v === null || v === "") return;
    lines.push(`${k}: ${v}`);
  };
  const list = (k: string, arr: string[]) => {
    if (!arr || arr.length === 0) { lines.push(`${k}: []`); return; }
    lines.push(`${k}:`);
    arr.forEach((v) => lines.push(`  - ${v}`));
  };
  const obj = (k: string, o: Record<string, unknown>) => {
    if (!o) return;
    lines.push(`${k}:`);
    Object.entries(o).forEach(([sk, sv]) => {
      if (sv !== undefined && sv !== null && sv !== "") lines.push(`  ${sk}: ${sv}`);
    });
  };

  simple("name", config.name);
  simple("description", config.description);
  simple("version", config.version);
  simple("tier", config.tier);
  if (config.runtime) {
    lines.push("");
    simple("runtime", config.runtime);
    if (config.runtime_config && Object.keys(config.runtime_config).length > 0) {
      obj("runtime_config", config.runtime_config as Record<string, unknown>);
    }
  }
  if (config.model) { lines.push(""); simple("model", config.model); }
  // Claude API primitives (issue #608)
  if (config.effort) { lines.push(""); simple("effort", config.effort); }
  if (config.task_budget && config.task_budget > 0) { simple("task_budget", config.task_budget); }
  if (config.prompt_files?.length) { lines.push(""); list("prompt_files", config.prompt_files); }
  lines.push(""); list("skills", config.skills);
  if (config.tools?.length) { list("tools", config.tools); }
  lines.push(""); obj("a2a", config.a2a as unknown as Record<string, unknown>);
  lines.push(""); obj("delegation", config.delegation as unknown as Record<string, unknown>);
  if (config.sandbox?.backend) { lines.push(""); obj("sandbox", config.sandbox as unknown as Record<string, unknown>); }

  return lines.join("\n") + "\n";
}
