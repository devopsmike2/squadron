export type AlertSeverity = "info" | "warning" | "critical";

export type ThresholdOperator = ">" | ">=" | "<" | "<=" | "==" | "!=";

export interface AlertRule {
  id: string;
  name: string;
  description?: string;
  query: string;
  threshold_operator: ThresholdOperator;
  threshold_value: number;
  interval_seconds: number;
  severity: AlertSeverity;
  enabled: boolean;
  webhook_url?: string;
  created_at: string;
  updated_at: string;
}

export interface AlertRuleInput {
  name: string;
  description: string;
  query: string;
  threshold_operator: ThresholdOperator;
  threshold_value: number;
  interval_seconds: number;
  severity: AlertSeverity;
  enabled: boolean;
  webhook_url: string;
}
