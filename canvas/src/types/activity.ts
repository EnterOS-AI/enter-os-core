export interface ActivityEntry {
  id: string;
  workspace_id: string;
  activity_type: string;
  source_id: string | null;
  target_id: string | null;
  method: string | null;
  summary: string | null;
  request_body: Record<string, unknown> | null;
  response_body: Record<string, unknown> | null;
  duration_ms: number | null;
  status: string;
  error_detail: string | null;
  created_at: string;
}
