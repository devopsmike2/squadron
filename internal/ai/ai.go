// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package ai is the v0.26+ AI-assist service. It wraps the
// Anthropic Messages API and exposes a small set of methods the
// rest of Squadron uses to enhance the operator experience:
//
//   - ExplainSnippet: take a recommendation snippet and produce a
//     2-3 sentence plain-English explanation of what it does and
//     why it would help.
//   - MergeIntoConfig: take an existing collector config + a
//     recommendation snippet and produce a merged YAML the
//     operator can preview, lint, and roll out.
//   - ExplainConfig: take a full collector config and summarize
//     what each pipeline does. Useful when the operator inherits
//     a config they didn't write.
//
// Every call is user-initiated; this package does no background
// or proactive work. Operator privacy + cost predictability beat
// a few proactive-LLM smarts.
//
// Direct HTTP, no SDK. The Anthropic Messages API is small enough
// that a dependency would be more code than the integration. The
// HTTP client is wrapped with otelhttp so AI requests show up in
// the operator's tracing alongside everything else Squadron does.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
)

// Default Anthropic model strings. Kept here (and used by Config
// defaults) so a model migration is one-file. Sonnet for merges
// (better at structural reasoning over YAML) and Haiku for short
// explanations (cheap and fast enough that operators don't
// hesitate to click).
const (
	DefaultBaseURL      = "https://api.anthropic.com"
	DefaultExplainModel = "claude-haiku-4-5-20251001"
	DefaultMergeModel   = "claude-sonnet-4-6"
	DefaultMaxTokens    = 1024

	apiVersion       = "2023-06-01"
	defaultUserAgent = "squadron/v0.26 (+https://github.com/devopsmike2/squadron)"

	// requestTimeout is the per-call HTTP timeout. Anthropic's
	// median latency at the small token counts we use is well
	// under 5s, so 30s leaves comfortable headroom for tail and
	// keeps the UI from spinning forever if something gets stuck.
	requestTimeout = 30 * time.Second
)

// Config is what the operator sets in squadron.yaml (or via the
// env var fallback for the API key). Mirrors the TelemetryConfig
// shape — master Enabled toggle, then sub-fields. Defaults are
// applied at NewService construction time so the operator can
// omit anything they don't care about overriding.
type Config struct {
	Enabled      bool   `yaml:"enabled"`
	APIKey       string `yaml:"api_key"`
	APIKeyEnv    string `yaml:"api_key_env"`
	BaseURL      string `yaml:"base_url"`
	ExplainModel string `yaml:"explain_model"`
	MergeModel   string `yaml:"merge_model"`
	MaxTokens    int    `yaml:"max_tokens"`
}

// Service wraps the Anthropic HTTP client + prompt templates.
// Stateless beyond the config; safe to share across requests.
type Service struct {
	cfg    Config
	client *http.Client
	logger *zap.Logger
}

// ErrDisabled is returned by every public method when the service
// was constructed without a valid API key. Handlers translate this
// into a 503 with a clear "AI assist not configured" message so
// the UI can render an opt-in nudge instead of a generic error.
var ErrDisabled = errors.New("ai service disabled (no API key configured)")

// NewService builds the service. Applies defaults to any unset
// Config fields. Returns a usable Service even when disabled —
// every method short-circuits with ErrDisabled so callers don't
// have to nil-check.
func NewService(cfg Config, logger *zap.Logger) *Service {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.ExplainModel == "" {
		cfg.ExplainModel = DefaultExplainModel
	}
	if cfg.MergeModel == "" {
		cfg.MergeModel = DefaultMergeModel
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	// otelhttp.NewTransport gives us automatic spans on every
	// Anthropic call. When selftel is disabled the propagator and
	// tracer are no-ops, so this is free.
	httpClient := &http.Client{
		Timeout: requestTimeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport,
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return "anthropic " + r.URL.Path
			}),
		),
	}
	return &Service{
		cfg:    cfg,
		client: httpClient,
		logger: logger,
	}
}

// Enabled reports whether the service has a usable API key. The
// handler trampoline calls this to decide between serving the
// route and 503'ing with the opt-in message.
func (s *Service) Enabled() bool {
	return s != nil && s.cfg.Enabled && s.cfg.APIKey != ""
}

// Capabilities is what /api/v1/ai/status returns. The UI uses it
// to decide which AI buttons to show — when AI is off the buttons
// stay hidden rather than appearing and immediately failing.
type Capabilities struct {
	Enabled      bool   `json:"enabled"`
	ExplainModel string `json:"explain_model,omitempty"`
	MergeModel   string `json:"merge_model,omitempty"`
}

// Capabilities returns the public view of the service's
// configuration. Never includes the API key.
func (s *Service) Capabilities() Capabilities {
	if !s.Enabled() {
		return Capabilities{Enabled: false}
	}
	return Capabilities{
		Enabled:      true,
		ExplainModel: s.cfg.ExplainModel,
		MergeModel:   s.cfg.MergeModel,
	}
}

// ----------------------------------------------------------------
// Public methods
// ----------------------------------------------------------------

// ExplainSnippetRequest is the input to ExplainSnippet. Signal +
// Goal are optional context that sharpens the explanation — pass
// them when the snippet came from a recommendation that knows
// them; leave blank otherwise.
type ExplainSnippetRequest struct {
	Snippet string `json:"snippet"`
	Signal  string `json:"signal,omitempty"`
	Goal    string `json:"goal,omitempty"`
}

// ExplainSnippetResponse is the output. The explanation is plain
// text (no markdown) intended to render directly in the
// recommendation card's expanded body.
type ExplainSnippetResponse struct {
	Explanation string `json:"explanation"`
	Model       string `json:"model"`
	TokensIn    int    `json:"tokens_in"`
	TokensOut   int    `json:"tokens_out"`
}

// ExplainSnippet asks the model to summarize what an OpenTelemetry
// Collector config snippet does. Uses the Explain model (Haiku by
// default) because the answer is short and structural.
func (s *Service) ExplainSnippet(ctx context.Context, req ExplainSnippetRequest) (*ExplainSnippetResponse, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(req.Snippet) == "" {
		return nil, errors.New("snippet is required")
	}
	system := `You are a senior OpenTelemetry Collector engineer. ` +
		`Given a YAML snippet from a Collector config (usually a single ` +
		`processor block plus a pipeline reference), explain in 2-3 plain ` +
		`English sentences what it does and what an operator gains by ` +
		`adding it. Do not include markdown, code fences, or headers. ` +
		`Be concrete about the byte/cardinality impact when the snippet ` +
		`implies one.`

	userMsg := buildExplainUserMessage(req)

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.ExplainModel,
		System:   system,
		UserText: userMsg,
	})
	if err != nil {
		return nil, fmt.Errorf("explain snippet: %w", err)
	}
	return &ExplainSnippetResponse{
		Explanation: strings.TrimSpace(resp.Text),
		Model:       resp.Model,
		TokensIn:    resp.TokensIn,
		TokensOut:   resp.TokensOut,
	}, nil
}

// MergeIntoConfigRequest is the input to MergeIntoConfig.
type MergeIntoConfigRequest struct {
	// BaseYAML is the operator's current collector config. We
	// don't validate it here — the operator is the source of
	// truth on what's "current".
	BaseYAML string `json:"base_yaml"`
	// SnippetYAML is the recommendation snippet to merge in.
	// Usually a processor block plus a pipeline reference;
	// the merge needs to add the processor under processors:
	// and append it to the named pipeline's processor list.
	SnippetYAML string `json:"snippet_yaml"`
	// Goal is short free text describing what the operator's
	// trying to do. Sharpens the merge ("drop attribute X" vs
	// "add a batch processor") without forcing us to parse the
	// snippet structurally.
	Goal string `json:"goal,omitempty"`
}

// MergeIntoConfigResponse returns the merged YAML plus a short
// changelog the operator can use to verify what changed before
// approving the rollout.
type MergeIntoConfigResponse struct {
	MergedYAML string `json:"merged_yaml"`
	Summary    string `json:"summary"` // short human-readable description of the change
	Model      string `json:"model"`
	TokensIn   int    `json:"tokens_in"`
	TokensOut  int    `json:"tokens_out"`
}

// MergeIntoConfig produces a merged collector config from the
// operator's base + a recommendation snippet. Uses the Merge model
// (Sonnet by default) because the output has to be syntactically
// and structurally correct YAML — Haiku is too risky for code
// generation at the sizes we'd give it.
//
// The Squadron config linter is the safety net here: any time the
// merged YAML is presented to the operator, it goes through the
// existing /api/v1/configs/lint endpoint and rolls out via the
// staged rollout flow. The LLM is one tool in a chain that
// already has rollback as a primitive.
func (s *Service) MergeIntoConfig(ctx context.Context, req MergeIntoConfigRequest) (*MergeIntoConfigResponse, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(req.BaseYAML) == "" {
		return nil, errors.New("base_yaml is required")
	}
	if strings.TrimSpace(req.SnippetYAML) == "" {
		return nil, errors.New("snippet_yaml is required")
	}

	system := `You are a senior OpenTelemetry Collector engineer. ` +
		`Merge a snippet into an existing Collector config. Output ONLY ` +
		`a JSON object with two string fields:` + "\n" +
		`  "merged_yaml": the complete merged YAML (no markdown, no code fences)` + "\n" +
		`  "summary": one sentence describing the change` + "\n" +
		`Rules: preserve every receiver, processor, exporter, extension, ` +
		`and connector that exists in the base. Add new components from ` +
		`the snippet under their correct top-level key. When the snippet ` +
		`references a pipeline (e.g. service.pipelines.metrics.processors), ` +
		`update that pipeline's processor list to include the new ` +
		`processor in a sensible position (typically right before the ` +
		`existing batch processor). Do not invent components that aren't ` +
		`in either input. Do not change anything the snippet doesn't ` +
		`speak to.`

	userMsg := buildMergeUserMessage(req)

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.MergeModel,
		System:   system,
		UserText: userMsg,
	})
	if err != nil {
		return nil, fmt.Errorf("merge into config: %w", err)
	}

	// The model's instructions tell it to return JSON. Parse;
	// fall back to treating the whole response as the YAML if the
	// model strayed (rare, but we'd rather show the operator
	// something usable than fail the request).
	type mergeBody struct {
		MergedYAML string `json:"merged_yaml"`
		Summary    string `json:"summary"`
	}
	var mb mergeBody
	if err := json.Unmarshal([]byte(extractJSONBlock(resp.Text)), &mb); err != nil || mb.MergedYAML == "" {
		s.logger.Warn("MergeIntoConfig: model did not return clean JSON; falling back",
			zap.Error(err), zap.Int("response_len", len(resp.Text)))
		mb.MergedYAML = strings.TrimSpace(resp.Text)
		mb.Summary = "Merged via AI assist (response shape was non-JSON; review carefully)."
	}
	return &MergeIntoConfigResponse{
		MergedYAML: mb.MergedYAML,
		Summary:    mb.Summary,
		Model:      resp.Model,
		TokensIn:   resp.TokensIn,
		TokensOut:  resp.TokensOut,
	}, nil
}

// ExplainConfigRequest is the input to ExplainConfig.
type ExplainConfigRequest struct {
	YAML string `json:"yaml"`
}

// ExplainConfigResponse returns a short summary plus per-pipeline
// notes. The handler returns the whole thing; the UI renders the
// summary first and lets the operator expand for the per-pipeline
// breakdown.
type ExplainConfigResponse struct {
	Summary   string            `json:"summary"`
	Pipelines map[string]string `json:"pipelines,omitempty"` // pipeline name → 1-line description
	Model     string            `json:"model"`
	TokensIn  int               `json:"tokens_in"`
	TokensOut int               `json:"tokens_out"`
}

// ExplainConfig produces a human-readable summary of an entire
// collector config. Useful when an operator inherits a config they
// didn't write — instead of reading 200 lines of YAML, they read
// "ingests OTLP, samples traces at 10%, drops k8s pod UIDs from
// metrics, exports to Honeycomb."
func (s *Service) ExplainConfig(ctx context.Context, req ExplainConfigRequest) (*ExplainConfigResponse, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(req.YAML) == "" {
		return nil, errors.New("yaml is required")
	}

	system := `You are a senior OpenTelemetry Collector engineer. ` +
		`Summarize what this collector config does. Output ONLY a JSON ` +
		`object with two fields:` + "\n" +
		`  "summary": 2-3 sentence overview` + "\n" +
		`  "pipelines": object mapping each pipeline name (e.g. "traces", ` +
		`"metrics", "logs/error_only") to a single sentence describing ` +
		`what that pipeline does` + "\n" +
		`Use concrete component names from the config. No markdown.`

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.ExplainModel, // Haiku is enough for summarization
		System:   system,
		UserText: "Collector config:\n```yaml\n" + req.YAML + "\n```",
	})
	if err != nil {
		return nil, fmt.Errorf("explain config: %w", err)
	}

	type body struct {
		Summary   string            `json:"summary"`
		Pipelines map[string]string `json:"pipelines"`
	}
	var b body
	if err := json.Unmarshal([]byte(extractJSONBlock(resp.Text)), &b); err != nil {
		s.logger.Warn("ExplainConfig: non-JSON response; falling back",
			zap.Error(err))
		b.Summary = strings.TrimSpace(resp.Text)
	}
	return &ExplainConfigResponse{
		Summary:   b.Summary,
		Pipelines: b.Pipelines,
		Model:     resp.Model,
		TokensIn:  resp.TokensIn,
		TokensOut: resp.TokensOut,
	}, nil
}

// ----------------------------------------------------------------
// v0.44 — Natural-language fleet query
// ----------------------------------------------------------------
//
// "Show me prod agents that haven't checked in for an hour" →
// structured GetAgentsParams. The model translates between the
// operator's mental model and the API the UI already speaks.
//
// We deliberately keep the output space tight — Claude returns ONLY
// fields the existing /agents endpoint already understands. New
// filter dimensions need a Squadron-side schema change first; the
// AI doesn't get to invent endpoints.

// FleetQueryRequest is the input. Schema is a per-deployment hint:
// available label keys, group names, etc. The model uses it to
// ground its answers (e.g. recognizing "Windows" maps to
// host.os.type=windows for THIS fleet's actual label vocabulary).
type FleetQueryRequest struct {
	Query  string      `json:"query"`
	Schema FleetSchema `json:"schema,omitempty"`
}

// FleetSchema is the small grounding hint passed alongside the
// query. All fields are optional — the model degrades gracefully.
type FleetSchema struct {
	LabelKeys []string `json:"label_keys,omitempty"`
	Groups    []string `json:"groups,omitempty"`
}

// FleetQueryResponse is the structured filter the UI applies. Mirror
// of GetAgentsParams + a short "explanation" line shown to the
// operator so they understand what the AI thought they meant.
type FleetQueryResponse struct {
	Status      string `json:"status,omitempty"`       // "online" | "offline" | "error"
	DriftStatus string `json:"drift_status,omitempty"` // "synced" | "drifted" | "no_intent" | "no_effective"
	GroupID     string `json:"group_id,omitempty"`
	Q           string `json:"q,omitempty"` // freetext search
	Explanation string `json:"explanation"`
	Model       string `json:"model"`
	TokensIn    int    `json:"tokens_in"`
	TokensOut   int    `json:"tokens_out"`
}

// TranslateFleetQuery asks Claude to convert a natural language
// query into structured filter params. The translation is
// constrained by a JSON schema so the model can't invent fields the
// UI doesn't know how to apply.
func (s *Service) TranslateFleetQuery(ctx context.Context, req FleetQueryRequest) (*FleetQueryResponse, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, errors.New("query is required")
	}

	system := `You translate plain-English fleet queries into Squadron filter parameters.

Output ONLY a JSON object with these fields (omit any you don't infer):
  "status":       "online" | "offline" | "error"
  "drift_status": "synced" | "drifted" | "no_intent" | "no_effective"
  "group_id":     a group name from the schema hint, or empty
  "q":            a freetext substring to match against agent name or label values
  "explanation":  one sentence telling the operator what you understood, in plain English

Rules:
- Use ONLY the four filter fields above. Do not invent endpoints.
- If the user mentions a state you can't infer, leave that field empty.
- The freetext "q" is your escape hatch for "name contains X" or "label value contains Y" patterns.
- Be conservative: it's better to under-filter (return more agents) than to misclassify a query and hide rows the operator wanted.`

	userMsg := "Query: " + strings.TrimSpace(req.Query)
	if len(req.Schema.LabelKeys) > 0 {
		userMsg += "\n\nAvailable label keys: " + strings.Join(req.Schema.LabelKeys, ", ")
	}
	if len(req.Schema.Groups) > 0 {
		userMsg += "\n\nGroup names: " + strings.Join(req.Schema.Groups, ", ")
	}

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.ExplainModel, // Haiku — query translation is short
		System:   system,
		UserText: userMsg,
	})
	if err != nil {
		return nil, fmt.Errorf("fleet query: %w", err)
	}

	type body struct {
		Status      string `json:"status"`
		DriftStatus string `json:"drift_status"`
		GroupID     string `json:"group_id"`
		Q           string `json:"q"`
		Explanation string `json:"explanation"`
	}
	var b body
	if err := json.Unmarshal([]byte(extractJSONBlock(resp.Text)), &b); err != nil {
		s.logger.Warn("TranslateFleetQuery: non-JSON response", zap.Error(err))
		// Defensive fallback: show the operator the raw text so they
		// can refine the question. They'll see this in the
		// explanation field.
		b.Explanation = strings.TrimSpace(resp.Text)
	}
	return &FleetQueryResponse{
		Status:      b.Status,
		DriftStatus: b.DriftStatus,
		GroupID:     b.GroupID,
		Q:           b.Q,
		Explanation: b.Explanation,
		Model:       resp.Model,
		TokensIn:    resp.TokensIn,
		TokensOut:   resp.TokensOut,
	}, nil
}

// ----------------------------------------------------------------
// v0.44 — Auto-remediate lint warnings
// ----------------------------------------------------------------
//
// Takes a collector config + the configlint findings and asks the
// model to return a fixed config. We constrain the change set
// strictly — the model is told to ONLY fix the listed warnings and
// leave everything else alone. Output is a JSON object with the
// remediated YAML + a one-line summary; the UI surfaces a diff so
// the operator reviews before saving.

type RemediateLintRequest struct {
	YAML     string        `json:"yaml"`
	Findings []LintFinding `json:"findings"`
}

// LintFinding mirrors internal/configlint.Finding's shape but lives
// here so the AI package doesn't depend on configlint. The handler
// layer adapts between the two.
type LintFinding struct {
	Severity string `json:"severity"` // "warning" | "error"
	Code     string `json:"code"`     // e.g. "localhost-exporter"
	Message  string `json:"message"`
	Path     string `json:"path,omitempty"` // YAML path the lint flagged
}

type RemediateLintResponse struct {
	FixedYAML string `json:"fixed_yaml"`
	Summary   string `json:"summary"`
	// Unaddressed lists findings the model declined to touch — usually
	// because the fix would change runtime behavior in a way the
	// operator should decide on (e.g. "we recommend deleting this
	// exporter but you might want it for an audit trail").
	Unaddressed []string `json:"unaddressed,omitempty"`
	Model       string   `json:"model"`
	TokensIn    int      `json:"tokens_in"`
	TokensOut   int      `json:"tokens_out"`
}

// RemediateLintWarnings asks Claude to apply targeted fixes for the
// supplied lint findings. The system prompt narrows the scope to
// "fix only what's listed" so the model doesn't gold-plate the
// config with unrelated changes.
func (s *Service) RemediateLintWarnings(ctx context.Context, req RemediateLintRequest) (*RemediateLintResponse, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(req.YAML) == "" {
		return nil, errors.New("yaml is required")
	}
	if len(req.Findings) == 0 {
		// Nothing to do — return the input as-is. Saves a round trip
		// when the UI fires Remediate on a clean config.
		return &RemediateLintResponse{
			FixedYAML: req.YAML,
			Summary:   "No lint findings to fix.",
		}, nil
	}

	system := `You are a senior OpenTelemetry Collector engineer. You fix lint warnings in collector configs.

Output ONLY a JSON object with these fields:
  "fixed_yaml":  the complete remediated YAML (no markdown, no code fences)
  "summary":     one sentence describing the change set as a whole
  "unaddressed": optional array of finding codes you declined to fix because
                 the fix would have changed runtime behavior in ways the
                 operator should decide

Rules:
- Fix ONLY the listed findings. Do not refactor unrelated sections.
- Preserve every receiver/processor/exporter/extension/connector and pipeline that exists in the original.
- Keep the YAML's structure and ordering as close to the original as you reasonably can.
- If a fix would be destructive (deleting an exporter, changing an endpoint to something other than what the warning recommended), put the finding's code in unaddressed and explain in the summary.`

	userMsg := "Config:\n```yaml\n" + req.YAML + "\n```\n\nLint findings to fix:\n"
	for _, f := range req.Findings {
		line := "- [" + f.Severity + "] " + f.Code + ": " + f.Message
		if f.Path != "" {
			line += " (at " + f.Path + ")"
		}
		userMsg += line + "\n"
	}

	resp, err := s.callMessages(ctx, callOpts{
		Model:    s.cfg.MergeModel, // Sonnet — touch real YAML carefully
		System:   system,
		UserText: userMsg,
	})
	if err != nil {
		return nil, fmt.Errorf("remediate lint: %w", err)
	}

	type body struct {
		FixedYAML   string   `json:"fixed_yaml"`
		Summary     string   `json:"summary"`
		Unaddressed []string `json:"unaddressed"`
	}
	var b body
	if err := json.Unmarshal([]byte(extractJSONBlock(resp.Text)), &b); err != nil || b.FixedYAML == "" {
		s.logger.Warn("RemediateLintWarnings: non-JSON response; surfacing raw",
			zap.Error(err), zap.Int("response_len", len(resp.Text)))
		b.FixedYAML = strings.TrimSpace(resp.Text)
		b.Summary = "Remediated via AI (response shape was non-JSON; review carefully)."
	}
	return &RemediateLintResponse{
		FixedYAML:   b.FixedYAML,
		Summary:     b.Summary,
		Unaddressed: b.Unaddressed,
		Model:       resp.Model,
		TokensIn:    resp.TokensIn,
		TokensOut:   resp.TokensOut,
	}, nil
}

// ----------------------------------------------------------------
// Anthropic HTTP plumbing
// ----------------------------------------------------------------

type callOpts struct {
	Model    string
	System   string
	UserText string
}

type callResp struct {
	Text      string
	Model     string
	TokensIn  int
	TokensOut int
}

// anthropicRequest mirrors the Anthropic Messages API request
// shape. Kept small — we don't use tool use, streaming, or vision
// in v0.26, and the JSON marshaling can always add fields later.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the slice of the Messages API response
// shape we actually use. Other fields (id, type, role, stop
// reasons) are ignored on decode.
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model string `json:"model"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (s *Service) callMessages(ctx context.Context, opts callOpts) (*callResp, error) {
	body := anthropicRequest{
		Model:     opts.Model,
		MaxTokens: s.cfg.MaxTokens,
		System:    opts.System,
		Messages: []anthropicMessage{
			{Role: "user", Content: opts.UserText},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.cfg.BaseURL, "/")+"/v1/messages",
		bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.cfg.APIKey)
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("User-Agent", defaultUserAgent)

	httpResp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic call: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode >= 400 {
		// Surface the Anthropic error message verbatim — it's
		// usually descriptive (rate limit, bad api key, model
		// not available) and an operator can act on it.
		return nil, fmt.Errorf("anthropic %d: %s", httpResp.StatusCode, string(truncate(raw, 500)))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if ar.Error != nil {
		return nil, fmt.Errorf("anthropic error: %s: %s", ar.Error.Type, ar.Error.Message)
	}
	if len(ar.Content) == 0 {
		return nil, errors.New("anthropic returned empty content")
	}

	// Concatenate text blocks (the API may return multiple but in
	// practice for our shapes there's one). Ignore non-text
	// blocks; we don't request tool use.
	var sb strings.Builder
	for _, block := range ar.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return &callResp{
		Text:      sb.String(),
		Model:     ar.Model,
		TokensIn:  ar.Usage.InputTokens,
		TokensOut: ar.Usage.OutputTokens,
	}, nil
}

// ----------------------------------------------------------------
// Prompt + parsing helpers
// ----------------------------------------------------------------

func buildExplainUserMessage(req ExplainSnippetRequest) string {
	var sb strings.Builder
	if req.Goal != "" {
		sb.WriteString("Context (what we're trying to do): ")
		sb.WriteString(req.Goal)
		sb.WriteString("\n\n")
	}
	if req.Signal != "" {
		sb.WriteString("Signal: ")
		sb.WriteString(req.Signal)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Snippet:\n```yaml\n")
	sb.WriteString(req.Snippet)
	sb.WriteString("\n```")
	return sb.String()
}

func buildMergeUserMessage(req MergeIntoConfigRequest) string {
	var sb strings.Builder
	if req.Goal != "" {
		sb.WriteString("Goal: ")
		sb.WriteString(req.Goal)
		sb.WriteString("\n\n")
	}
	sb.WriteString("BASE CONFIG (the operator's current effective config):\n")
	sb.WriteString("```yaml\n")
	sb.WriteString(req.BaseYAML)
	sb.WriteString("\n```\n\n")
	sb.WriteString("SNIPPET TO MERGE:\n```yaml\n")
	sb.WriteString(req.SnippetYAML)
	sb.WriteString("\n```\n\n")
	sb.WriteString("Return the JSON object as instructed.")
	return sb.String()
}

// extractJSONBlock pulls a JSON object out of a response that may
// be wrapped in markdown code fences. Models are usually obedient
// when told to skip fences, but defensive parsing here saves the
// operator from a hard failure when the model occasionally adds
// ```json ... ``` despite the system prompt.
func extractJSONBlock(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Strip leading ```json or ``` and trailing ```.
		idx := strings.Index(s, "\n")
		if idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

// truncate returns at most n bytes of b, suitable for embedding in
// an error message without flooding the log.
func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append(b[:n:n], []byte("…")...)
}
