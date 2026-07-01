// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package placement turns a recommendation kind into a ranked list of
// suggested file paths in an operator's connected Terraform repo.
//
// The open-PR flow resolves where to write a fix from the connection's
// static placement map (resource_kind -> file path). When no row matches
// a kind, the PR dead-ends with NoPlacementMapping and the operator has
// to hand-configure a path. This package removes that dead-end: given the
// kind and a deterministic summary of the repo's .tf files
// (internal/iac/hclsummary), it suggests where the fix most likely
// belongs — ranked, with a human reason — so the UI can offer "use this
// path" instead of an empty error.
//
// It is pure and deterministic: no AI, no network. The caller fetches the
// repo summaries; this package only ranks.
package placement

import (
	"path"
	"sort"
	"strings"

	"github.com/devopsmike2/squadron/internal/iac/hclsummary"
)

// Suggestion is one ranked placement candidate.
type Suggestion struct {
	// Path is the repo-relative file path (existing file, or a
	// conventional new-file name when NewFile is true).
	Path string `json:"path"`
	// Score is the relative confidence (higher is better). Callers
	// should treat it as an ordering key, not an absolute probability.
	Score int `json:"score"`
	// Reason is a short, operator-facing justification.
	Reason string `json:"reason"`
	// NewFile is true when Path does not exist in the repo and the
	// suggestion is to create it (a conventional fallback).
	NewFile bool `json:"new_file,omitempty"`
}

// scoring tiers — kept as named constants so the ranking is auditable.
const (
	scoreDeclaresType    = 100 // file declares a resource of a target type
	scoreFilenameHint    = 50  // filename hints at the resource family
	scoreConventional    = 20  // generic catch-all file (main.tf, resources.tf)
	scoreNewFileFallback = 5   // synthetic create-this-file suggestion
)

// maxSuggestions caps the returned list so the UI stays readable.
const maxSuggestions = 5

// conventionalFiles are catch-all Terraform files a fix can reasonably
// land in when nothing more specific matches.
var conventionalFiles = map[string]struct{}{
	"main.tf":          {},
	"resources.tf":     {},
	"observability.tf": {},
	"monitoring.tf":    {},
}

// KindToResourceTypes maps a recommendation kind to the Terraform
// resource type(s) a fix for it sits next to / references. The first
// element is the primary type (used to derive the new-file fallback
// name). Mirrors the inverse of classifyResourceKind; kept in sync with
// internal/iac.KindDispositions by TestEveryDispositionKindIsMapped.
func KindToResourceTypes(kind string) []string {
	switch kind {
	// --- AWS ---
	case "s3-access-logging":
		return []string{"aws_s3_bucket", "aws_s3_bucket_logging"}
	case "alb-access-logs":
		return []string{"aws_lb", "aws_alb"}
	case "ec2-otel-layer":
		return []string{"aws_instance", "aws_ssm_association", "aws_ssm_document"}
	case "lambda-otel-layer":
		return []string{"aws_lambda_function", "aws_lambda_layer_version"}
	case "rds-pi-em":
		return []string{"aws_db_instance", "aws_rds_cluster"}
	case "dynamodb-contributor-insights":
		return []string{"aws_dynamodb_table", "aws_dynamodb_contributor_insights"}
	case "ecs-container-insights":
		return []string{"aws_ecs_cluster"}
	case "eks-cluster-logging":
		return []string{"aws_eks_cluster"}
	case "eks-observability-addon":
		return []string{"aws_eks_addon", "aws_eks_cluster"}
	case "eventbridge-logging-enable":
		return []string{"aws_cloudwatch_event_rule", "aws_cloudwatch_event_target", "aws_cloudwatch_event_bus"}
	case "eventbridge-rule-preserves-trace":
		return []string{"aws_cloudwatch_event_target", "aws_cloudwatch_event_rule"}
	case "eventbridge-schemas-discover":
		return []string{"aws_schemas_discoverer", "aws_cloudwatch_event_bus"}
	case "sqs-redrive-policy-enable":
		return []string{"aws_sqs_queue"}
	case "sns-delivery-logging-enable":
		return []string{"aws_sns_topic"}
	// --- GCP ---
	case "gcs-logging-enable":
		return []string{"google_storage_bucket"}
	case "gclb-logging-enable":
		return []string{"google_compute_backend_service"}
	case "cloudsql-pi-enable":
		return []string{"google_sql_database_instance"}
	case "gke-mp-enable":
		return []string{"google_container_cluster", "google_container_node_pool"}
	case "pubsub-subscription-preserves-attrs":
		return []string{"google_pubsub_subscription"}
	case "pubsub-schema-attach":
		return []string{"google_pubsub_schema", "google_pubsub_topic"}
	case "pubsub-trace-enable":
		return []string{"google_pubsub_topic"}
	case "cloudtasks-retry-policy-enable", "cloudtasks-logging-enable":
		return []string{"google_cloud_tasks_queue"}
	case "pubsublite-logging-enable", "pubsublite-reservation-attach":
		return []string{"google_pubsub_lite_topic"}
	case "gce-otel-label":
		return []string{"google_compute_instance"}
	// --- Azure ---
	case "azblob-diag-enable":
		return []string{"azurerm_storage_account"}
	case "azlb-diag-enable":
		return []string{"azurerm_lb"}
	case "azsql-diag-enable":
		return []string{"azurerm_mssql_database", "azurerm_sql_database"}
	case "aks-monitor-enable":
		return []string{"azurerm_kubernetes_cluster"}
	case "vm-otel-tag":
		return []string{"azurerm_linux_virtual_machine", "azurerm_windows_virtual_machine", "azurerm_virtual_machine"}
	case "servicebus-diagnostics-enable":
		return []string{"azurerm_servicebus_namespace"}
	case "servicebus-policy-preserves-traceparent":
		return []string{"azurerm_servicebus_namespace_authorization_rule"}
	case "eventgrid-diagnostics-enable", "eventgrid-cloudevent-schema-enforce":
		return []string{"azurerm_eventgrid_topic"}
	case "eventhubs-diagnostics-enable":
		return []string{"azurerm_eventhub_namespace"}
	case "eventhubs-capture-enable":
		return []string{"azurerm_eventhub"}
	// --- OCI ---
	case "ocibucket-logging-enable":
		return []string{"oci_objectstorage_bucket"}
	case "ocilb-logging-enable":
		return []string{"oci_load_balancer"}
	case "ocidb-perfhub-enable":
		return []string{"oci_database_autonomous_database", "oci_database_database"}
	case "oke-ops-insights-enable":
		return []string{"oci_containerengine_cluster"}
	case "compute-otel-tag":
		return []string{"oci_core_instance"}
	case "streaming-config-preserves-headers":
		return []string{"oci_streaming_stream"}
	case "streaming-logging-enable":
		return []string{"oci_streaming_stream", "oci_logging_log"}
	case "ons-logging-enable":
		return []string{"oci_ons_notification_topic"}
	case "queues-logging-enable":
		return []string{"oci_queue_queue", "oci_logging_log"}
	// --- Serverless regression recs (detection→proposal). Resource types
	// mirror the iacpicker cold-start / error-rate snippets so open-PR can
	// suggest the placement file that holds the function / service. ---
	case "lambda-cold-start-baseline":
		// New aws_lambda_provisioned_concurrency_config referencing the function.
		return []string{"aws_lambda_provisioned_concurrency_config", "aws_lambda_function"}
	case "cloudrun-cold-start-baseline":
		return []string{"google_cloud_run_service"}
	case "cloudfunc-cold-start-baseline":
		return []string{"google_cloudfunctions2_function"}
	case "azfunc-cold-start-baseline":
		return []string{"azurerm_service_plan", "azurerm_linux_function_app"}
	case "ocifunc-cold-start-baseline":
		return []string{"oci_functions_function"}
	case "span-quality-error-rate-spike":
		// Cross-cloud kind: one of five surfaces fires per rec, so the
		// union lets the placement matcher land on whichever exists in
		// the repo. Primary (Lambda) drives the new-file fallback name.
		return []string{
			"aws_lambda_function", "google_cloud_run_service",
			"google_cloudfunctions2_function", "azurerm_service_plan",
			"oci_functions_function",
		}
	}
	return nil
}

// providerPrefixes are stripped when deriving filename hint tokens from a
// resource type, so "google_storage_bucket" yields {storage, bucket}.
var providerPrefixes = []string{"aws_", "google_", "azurerm_", "oci_"}

// hintTokens derives lowercase filename-hint tokens from a kind's
// resource types (e.g. google_storage_bucket -> storage, bucket). Common
// noise tokens are dropped so a hint actually narrows the field.
func hintTokens(kind string) []string {
	noise := map[string]struct{}{
		"resource": {}, "cluster": {}, "instance": {}, "database": {},
		"service": {}, "rule": {}, "target": {}, "policy": {}, "namespace": {},
		"core": {}, "log": {}, "logging": {}, "version": {}, "function": {},
		"authorization": {}, "compute": {},
	}
	seen := map[string]struct{}{}
	var out []string
	for _, rt := range KindToResourceTypes(kind) {
		t := rt
		for _, p := range providerPrefixes {
			t = strings.TrimPrefix(t, p)
		}
		for _, tok := range strings.Split(t, "_") {
			if len(tok) < 2 {
				continue
			}
			if _, bad := noise[tok]; bad {
				continue
			}
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			out = append(out, tok)
		}
	}
	return out
}

// resourceType extracts the type from an hclsummary resource address
// ("aws_s3_bucket.logs" -> "aws_s3_bucket").
func resourceType(addr string) string {
	if i := strings.IndexByte(addr, '.'); i > 0 {
		return addr[:i]
	}
	return addr
}

// Suggest ranks the repo's files for where a fix of the given kind
// should go. files is the deterministic summary of the repo's .tf files
// (parsed or not — an unparsed file still contributes its filename).
// Returns at most maxSuggestions, best first. When nothing in the repo
// matches, returns a single synthetic new-file suggestion so the caller
// always has something actionable.
func Suggest(kind string, files []hclsummary.FileSummary) []Suggestion {
	types := KindToResourceTypes(kind)
	typeSet := make(map[string]struct{}, len(types))
	for _, t := range types {
		typeSet[t] = struct{}{}
	}
	hints := hintTokens(kind)

	best := map[string]Suggestion{} // path -> best suggestion for it
	consider := func(s Suggestion) {
		if cur, ok := best[s.Path]; !ok || s.Score > cur.Score {
			best[s.Path] = s
		}
	}

	for _, f := range files {
		base := strings.ToLower(path.Base(f.Path))

		// Tier 1: the file declares a resource of a target type.
		matchedType := ""
		for _, addr := range f.Resources {
			if _, ok := typeSet[resourceType(addr)]; ok {
				matchedType = resourceType(addr)
				break
			}
		}
		if matchedType != "" {
			consider(Suggestion{Path: f.Path, Score: scoreDeclaresType,
				Reason: "declares " + matchedType})
			continue
		}

		// Tier 2: the filename hints at the resource family.
		hit := ""
		for _, tok := range hints {
			if strings.Contains(base, tok) {
				hit = tok
				break
			}
		}
		if hit != "" {
			consider(Suggestion{Path: f.Path, Score: scoreFilenameHint,
				Reason: "filename matches " + hit})
			continue
		}

		// Tier 3: a conventional catch-all file.
		if _, ok := conventionalFiles[base]; ok {
			consider(Suggestion{Path: f.Path, Score: scoreConventional,
				Reason: "conventional Terraform file"})
		}
	}

	out := make([]Suggestion, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		di, dj := strings.Count(out[i].Path, "/"), strings.Count(out[j].Path, "/")
		if di != dj {
			return di < dj // shallower first
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}

	// Nothing matched — offer a conventional new file so the caller
	// always has an actionable default.
	if len(out) == 0 {
		out = append(out, Suggestion{
			Path:    newFileName(kind),
			Score:   scoreNewFileFallback,
			Reason:  "no existing match — suggested new file",
			NewFile: true,
		})
	}
	return out
}

// newFileName derives a conventional new-file name for a kind from its
// primary hint token (e.g. gcs-logging-enable -> storage.tf), falling
// back to a generic observability file.
func newFileName(kind string) string {
	if toks := hintTokens(kind); len(toks) > 0 {
		return toks[0] + ".tf"
	}
	return "squadron-observability.tf"
}
