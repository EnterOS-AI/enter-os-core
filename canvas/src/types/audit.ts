/** Audit ledger entry — issued by GET /workspaces/:id/audit */
export interface AuditEntry {
  id: string;
  workspace_id: string;
  event_type: "delegation" | "decision" | "gate" | "hitl";
  actor: string;
  summary: string;
  chain_valid: boolean;
  created_at: string;
}

/** Paginated response envelope from GET /workspaces/:id/audit */
export interface AuditResponse {
  entries: AuditEntry[];
  /** Opaque cursor for the next page; null when no more pages exist. */
  cursor: string | null;
}
