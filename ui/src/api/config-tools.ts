// Squadron server-side config tooling — lint and templates.
// Note: simpleRequest already prefixes apiBaseUrl (which ends in /api/v1),
// so paths here start at the resource (/configs/...).

import { simpleRequest } from "./base";

import type {
  ConfigTemplate,
  LintFinding,
  LintResponse,
} from "@/types/config-tools";

export const lintConfig = async (content: string): Promise<LintFinding[]> => {
  const resp = await simpleRequest<LintResponse>("/configs/lint", {
    method: "POST",
    body: JSON.stringify({ content }),
  });
  return resp.findings ?? [];
};

interface TemplatesResponse {
  templates: ConfigTemplate[];
}

export const listConfigTemplates = async (): Promise<ConfigTemplate[]> => {
  const resp = await simpleRequest<TemplatesResponse>("/configs/templates");
  return resp.templates ?? [];
};

export const getConfigTemplate = async (
  id: string,
): Promise<ConfigTemplate> => {
  return simpleRequest<ConfigTemplate>(`/configs/templates/${id}`);
};
