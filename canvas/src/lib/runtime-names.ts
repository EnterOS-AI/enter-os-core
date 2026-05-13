// Friendly display names for workspace runtimes.
// Used in the chat indicator, details tab, and anywhere else we surface
// the runtime to the user.

const RUNTIME_NAMES: Record<string, string> = {
  "claude-code": "Claude Code",
  langgraph: "LangGraph",
  deepagents: "DeepAgents",
  openclaw: "OpenClaw",
  crewai: "CrewAI",
  autogen: "AutoGen",
  kimi: "Kimi",
  "kimi-cli": "Kimi CLI",
};

export function runtimeDisplayName(runtime: string): string {
  return RUNTIME_NAMES[runtime] || runtime || "agent";
}
