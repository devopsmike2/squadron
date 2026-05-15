import { simpleRequest } from "./base";

import type { AlertRule, AlertRuleInput } from "@/types/alert";

interface ListResponse {
  rules: AlertRule[];
}

export const listAlertRules = async (): Promise<AlertRule[]> => {
  const data = await simpleRequest<ListResponse>("/alerts/rules");
  return data.rules ?? [];
};

export const getAlertRule = async (id: string): Promise<AlertRule> => {
  return simpleRequest<AlertRule>(`/alerts/rules/${id}`);
};

export const createAlertRule = async (
  input: AlertRuleInput,
): Promise<AlertRule> => {
  return simpleRequest<AlertRule>("/alerts/rules", {
    method: "POST",
    body: JSON.stringify(input),
  });
};

export const updateAlertRule = async (
  id: string,
  input: AlertRuleInput,
): Promise<AlertRule> => {
  return simpleRequest<AlertRule>(`/alerts/rules/${id}`, {
    method: "PUT",
    body: JSON.stringify(input),
  });
};

export const deleteAlertRule = async (id: string): Promise<void> => {
  await simpleRequest<void>(`/alerts/rules/${id}`, {
    method: "DELETE",
  });
};
