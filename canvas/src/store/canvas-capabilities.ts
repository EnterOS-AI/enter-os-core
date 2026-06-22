import type { WorkspaceNodeData } from "./canvas";
import { extractSkillNames } from "./canvas-topology";

export interface WorkspaceCapabilitySummary {
  runtime: string | null;
  skills: string[];
  skillCount: number;
  currentTask: string;
  hasActiveTask: boolean;
}

/**
 * Derives a capability summary from workspace node data.
 */
export function summarizeWorkspaceCapabilities(data: WorkspaceNodeData): WorkspaceCapabilitySummary {
  const skills = extractSkillNames(data.agentCard);
  const currentTask = data.currentTask.trim();
  // Prefer workspace-level runtime (from DB, always set) over agentCard.runtime (set by agent on register)
  const runtime = (typeof data.runtime === "string" && data.runtime) ? data.runtime
    : (typeof data.agentCard?.runtime === "string" ? data.agentCard.runtime : null);

  return {
    runtime,
    skills,
    skillCount: skills.length,
    currentTask,
    hasActiveTask: currentTask.length > 0,
  };
}
