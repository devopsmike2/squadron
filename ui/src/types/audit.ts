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
}

export interface AuditEventFilter {
  target_type?: string;
  target_id?: string;
  since?: string; // RFC3339
  limit?: number;
}
