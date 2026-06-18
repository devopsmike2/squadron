// Wire types for Squadron's audit log. Mirror of services.AuditEvent.

export interface AuditEvent {
  id: string;
  timestamp: string; // RFC3339
  actor: string;
  event_type: string;
  target_type: string;
  target_id?: string;
  action: string;
  payload?: Record<string, unknown>;
  created_at: string;
  // v0.57 — cached AI explanation. Empty until the operator clicks
  // Explain on the row; populated server side and persisted on the
  // audit row so subsequent reads do not re-query the LLM.
  ai_explanation?: string;
  ai_explanation_model?: string;
  ai_explanation_generated_at?: string;
}

// AuditExplainResponse mirrors the JSON body returned by
// POST /api/v1/audit/:id/explain. cached=true means the LLM was not
// called for this request.
export interface AuditExplainResponse {
  explanation: string;
  model: string;
  generated_at: string;
  cached: boolean;
  redaction_summary?: string;
}

export interface AuditEventFilter {
  target_type?: string;
  target_id?: string;
  since?: string; // RFC3339
  limit?: number;
}
