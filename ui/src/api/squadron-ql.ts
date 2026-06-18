import { apiPost, apiGet, apiPut, apiDelete } from "./base";

export interface SquadronQLRequest {
  query: string;
  start_time?: string;
  end_time?: string;
  limit?: number;
  agent_id?: string;
  group_id?: string;
}

export interface QueryResult {
  type: "metrics" | "logs" | "traces";
  timestamp: string;
  labels: Record<string, string>;
  value: unknown;
  data?: Record<string, unknown>;
}

export interface QueryMeta {
  execution_time: number;
  row_count: number;
  query_type: string;
  used_rollups: boolean;
}

export interface SquadronQLResponse {
  results: QueryResult[];
  meta: QueryMeta;
}

export interface ValidateQueryResponse {
  valid: boolean;
  error?: string;
  message?: string;
}

export interface SuggestionsResponse {
  suggestions: string[];
}

export interface QueryTemplate {
  id: string;
  name: string;
  description: string;
  query: string;
  category: string;
}

export interface TemplatesResponse {
  templates: QueryTemplate[];
}

export interface FunctionInfo {
  name: string;
  description: string;
  example: string;
}

export interface FunctionsResponse {
  functions: FunctionInfo[];
}

/**
 * Execute a Squadron QL query
 */
export async function executeSquadronQL(
  request: SquadronQLRequest,
): Promise<SquadronQLResponse> {
  return apiPost<SquadronQLResponse>("/telemetry/query", request);
}

/**
 * Validate a Squadron QL query
 */
export async function validateQuery(
  query: string,
): Promise<ValidateQueryResponse> {
  return apiPost<ValidateQueryResponse>("/telemetry/query/validate", { query });
}

/**
 * Get query suggestions for auto-completion
 */
export async function getQuerySuggestions(
  query: string,
  cursorPos: number,
): Promise<SuggestionsResponse> {
  return apiPost<SuggestionsResponse>("/telemetry/query/suggestions", {
    query,
    cursor_pos: cursorPos,
  });
}

/**
 * Get query templates
 */
export async function getQueryTemplates(): Promise<TemplatesResponse> {
  return apiGet<TemplatesResponse>("/telemetry/query/templates");
}

/**
 * Get available functions
 */
export async function getQueryFunctions(): Promise<FunctionsResponse> {
  return apiGet<FunctionsResponse>("/telemetry/query/functions");
}

export interface SavedQuery {
  id: string;
  name: string;
  description: string;
  query: string;
  tags: string[];
  created_at: string;
  updated_at: string;
}

export interface SavedQueriesResponse {
  saved_queries: SavedQuery[];
}

export interface SavedQueryInput {
  name: string;
  description?: string;
  query: string;
  tags?: string[];
}

export async function getSavedQueries(): Promise<SavedQueriesResponse> {
  return apiGet<SavedQueriesResponse>("/telemetry/saved-queries");
}

export async function createSavedQueryApi(
  input: SavedQueryInput,
): Promise<SavedQuery> {
  return apiPost<SavedQuery>("/telemetry/saved-queries", input);
}

export async function updateSavedQueryApi(
  id: string,
  input: SavedQueryInput,
): Promise<SavedQuery> {
  return apiPut<SavedQuery>(`/telemetry/saved-queries/${id}`, input);
}

export async function deleteSavedQueryApi(id: string): Promise<void> {
  return apiDelete<void>(`/telemetry/saved-queries/${id}`);
}
