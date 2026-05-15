// Wire types for Squadron's server-side config tooling.
// Mirror of internal/configlint and internal/configtemplates Go structs.

export type LintSeverity = "error" | "warning" | "info";

export interface LintFinding {
  severity: LintSeverity;
  rule: string;
  message: string;
  /** 1-indexed line in the source YAML; 0 if unknown. */
  line?: number;
  /** Dotted path through the YAML tree, e.g. "service.pipelines.traces". */
  path?: string;
}

export interface LintResponse {
  findings: LintFinding[];
}

export interface ConfigTemplate {
  id: string;
  name: string;
  description: string;
  category: string;
  yaml: string;
}
