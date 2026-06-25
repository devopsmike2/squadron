// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	awsorch "github.com/devopsmike2/squadron/internal/discovery/aws"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/iac"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/proposer/checkrunprompt"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DiscoveryAIProposer is the slim contract HandleAWSGenerateRecommendations
// calls against the AI proposer. Mirrors the per-handler interface
// pattern the rest of this file uses: the production wire passes
// *ai.Service directly (it satisfies the interface); tests substitute a
// stub that returns a pre-canned ProposalResult without touching the
// Anthropic SDK.
type DiscoveryAIProposer interface {
	ProposeFromDiscoveryScan(ctx context.Context, in *ai.DiscoveryScanContext) (*ai.ProposalResult, error)
}

// DiscoveryValidator is the slim contract HandleAWSValidate calls
// against. The production wire builds an *aws.Scanner per request and
// passes it via the factory; tests substitute a mock validator so the
// handler can be exercised without any AWS SDK code paths or import
// graph weight.
//
// Validator implementations MUST NOT persist anything — the wizard
// calls this before the connection is real. Auditing of a successful
// connect happens at the (later) Save endpoint, which Squadron-wide
// slice-1 work hasn't reached yet.
type DiscoveryValidator interface {
	Validate(ctx context.Context, conn *credstore.CloudConnection) (*scanner.ValidationResult, error)
}

// AWSValidatorFactory builds a DiscoveryValidator from the wizard's
// per-request inputs. Indirected so the handler doesn't need to know
// how to call sts:AssumeRole — production wires the AWS scanner;
// tests wire a mock.
type AWSValidatorFactory func(creds credstore.AWSCredentials, accountID string) DiscoveryValidator

// DiscoveryScanner is the slim contract HandleAWSRunScan calls
// against. It mirrors the Scan portion of scanner.Scanner; the handler
// uses an interface rather than the concrete *aws.Scanner so tests can
// inject a stub without dragging in the AWS SDK.
//
// Implementations are constructed per request through AWSScannerFactory
// — production decrypts the connection's credentials via the credstore
// Key and hands back an *aws.Scanner; tests return a pre-canned result.
type DiscoveryScanner interface {
	Scan(ctx context.Context, conn *credstore.CloudConnection, regions []string) (*scanner.Result, error)
}

// OrchestrationDiscoveryScanner is the optional orchestration-tier
// extension to DiscoveryScanner. Slice 1 chunk 1 of the orchestration-
// tier arc (v0.89.95, #728 Stream 126). Scanners that implement this
// interface get dispatched a per-call ScanOrchestrations after the
// main Scan returns when the request's Tiers include "orchestration";
// scanners that don't surface no orchestration rows on the response.
//
// Kept as a separate interface (rather than appended to DiscoveryScanner)
// so the GCP / Azure / OCI scanner types that haven't yet grown an
// orchestration surface continue to satisfy DiscoveryScanner without
// having to ship a no-op ScanOrchestrations. The handler does a runtime
// type assertion; failure to satisfy the interface is silently treated
// as "no orchestration support" — matches the chunk-2/3 staged rollout
// where each cloud lights up incrementally.
type OrchestrationDiscoveryScanner interface {
	ScanOrchestrations(ctx context.Context, scope scanner.ScanScope) ([]scanner.OrchestrationInstanceSnapshot, error)
}

// EventSourceDiscoveryScanner is the optional event-source-tier extension
// to DiscoveryScanner. Slice 1 chunk 1 of the event-source-tier arc
// (v0.89.100, #734 Stream 132). Mirrors OrchestrationDiscoveryScanner —
// scanners that implement this interface get dispatched a per-call
// ScanEventSources after the main Scan returns when the request's Tiers
// include "event_source"; scanners that don't surface no event source
// rows on the response.
//
// Kept as a separate interface (rather than appended to DiscoveryScanner)
// so the GCP / Azure / OCI scanner types that haven't yet grown an event
// source surface continue to satisfy DiscoveryScanner without having to
// ship a no-op ScanEventSources. The handler does a runtime type
// assertion; failure to satisfy the interface is silently treated as "no
// event source support" — matches the chunks-2/3/4 staged rollout where
// each cloud lights up incrementally.
type EventSourceDiscoveryScanner interface {
	ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error)
}

// AWSScannerFactory builds a DiscoveryScanner from a stored connection.
// Indirected so the scan handler doesn't import the AWS SDK directly;
// the production factory (defaultAWSScannerFactory in
// discovery_aws_wire.go) closes over the credstore Key supplied via
// WithCredstoreKey. A nil factory means "scan endpoint not wired" and
// the handler 500s with a humanized error.
type AWSScannerFactory func(conn *credstore.CloudConnection) (DiscoveryScanner, error)

// DiscoveryHandlers serves the connector-wizard surface — slice 1
// ships AWS validate + save; future slices add GCP and Azure
// equivalents behind the same handler shape.
//
// credStore is consumed by HandleAWSSaveConnection (Save persists the
// trust-policy metadata). HandleAWSValidate does NOT touch the store
// — the validate endpoint creates zero records by design.
//
// auditService is consumed by HandleAWSSaveConnection to emit the
// discovery.aws.connection_created event. Optional at construction —
// a nil auditService means "no audit event emitted" rather than a
// 500. Discovery's other event types land on real persistence reads
// (credstore) and are emitted by the substrate, not the handler.
//
// awsKey is the credstore Key used to encrypt AWSCredentials at Save
// time. Wired by the trampoline from the substrate's active backend so
// the same key encrypts validate-flow-stored creds as the credstore
// itself uses when later scans decrypt them. Optional at construction
// for the same reason as auditService — discovery_test.go's mock path
// short-circuits the encryption via WithCredentialMarshaller below.
type DiscoveryHandlers struct {
	credStore credstore.Store
	// recJobs backs async discovery recommendations (v0.89.209). Never
	// nil after NewDiscoveryHandlers; the kick-off + poll handlers use it.
	recJobs           *recommendationJobStore
	awsValidatorFor   AWSValidatorFactory
	awsCredMarshaller AWSCredMarshaller
	awsScannerFor     AWSScannerFactory
	auditService      services.AuditService
	// aiProposer is the v0.85 Stream 2F discovery-side AI proposer.
	// Optional at construction: only HandleAWSGenerateRecommendations
	// consumes it; the wizard / list / scan endpoints don't. The
	// recommendations route returns 503 when this is nil so the rest
	// of the discovery surface stays reachable on AI-disabled
	// deployments.
	aiProposer DiscoveryAIProposer
	// v0.89.28 (#643 slice 1) — accepted-recommendations few-shot
	// loop. Optional: nil means "no learning signal," which produces
	// a cold-start prompt byte-for-byte identical to pre-v0.89.28.
	// The wiring layer constructs a *proposer.DiscoveryBridge over
	// the application store + iacconnstore.Store; production sets
	// it via WithAcceptedRecommendationsAssembler, tests substitute
	// a stub that returns pre-canned examples.
	acceptedAssembler DiscoveryAcceptedRecommendationsAssembler
	// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — operator-set
	// exclusion store. Optional: nil means the
	// HandleAWSRecommendationExclude handler 503s with a clear "not
	// wired" message. Production wires the application store directly
	// (which satisfies the slim DiscoveryExclusionStore interface);
	// tests substitute a fake.
	exclusionStore DiscoveryExclusionStore

	// v0.89.44 (#665 Stream 63, slice 1 chunk 4 of the GitHub Checks
	// API back-signal arc). All four fields are optional: when any one
	// is nil/empty the chunk-4 PATCH-to-neutral follow-up inside
	// HandleAWSRecommendationExclude is a no-op (fail-open per design
	// doc §5). This is the slice-1 posture for deployments that
	// haven't upgraded their PAT scope or wired the checks integration.
	// ChecksAPI + CheckRunStore are the slim interfaces defined in
	// iac_github_checkrun.go — the application store satisfies
	// CheckRunStore directly; *iacgithub.PATClient satisfies ChecksAPI
	// directly.
	checksClient  ChecksAPI
	checkRunStore CheckRunStore
	checksPAT     string
	squadronHost  string

	// traceIndex — v0.89.77 (#708 Stream 106, Trace integration
	// slice 1 chunk 4). Optional: nil leaves the scan response
	// LastSeenAt-unannotated (every row renders "never" in the UI),
	// which is the correct cold-start posture for a deployment that
	// hasn't observed any spans yet. Production wires the same
	// *traceindex.Index the chunk-2 OTLP receiver dispatches Observe
	// to and the chunk-3 Discovery dashboard reads Coverage from —
	// one index per Squadron process. A flaky index logs warnings
	// but never breaks the scan endpoint (see
	// AnnotateComputeWithLastSeen).
	traceIndex TraceIndexLookup

	// coldStartStore + coldStartConstants — Cold-start latency
	// analysis slice 1 chunk 3 (v0.89.115, #753 Stream 151). Both
	// optional: nil store OR nil constants short-circuits the
	// AnnotateServerlessWithColdStart pass entirely, leaving the
	// per-row cold_start_p95_ms + cold_start_exceeds_threshold
	// fields nil (rendered as "—" in the UI). Production wires the
	// chunk-1 *sqlite.Storage to the store and a
	// staticColdStartDetectionConstants pinned to the substrate
	// defaults (24h current / 168h baseline / 1.5x / 500ms). A
	// flaky storage layer logs warnings but never breaks the scan
	// endpoint (mirrors the traceIndex posture above).
	coldStartStore     ColdStartObservationReader
	coldStartConstants ColdStartAnnotationThresholds

	// errorRateStore — Error rate correlation slice 1 chunk 3
	// (v0.89.129, #769 Stream 167). Optional: nil store skips the
	// AnnotateServerlessWithErrorRate pass entirely, leaving the
	// per-row current_error_rate + error_rate_exceeds_threshold
	// fields nil (rendered as "—" in the UI). Same nil-tolerant
	// posture as coldStartStore.
	errorRateStore ErrorRateObservationStore

	logger *zap.Logger
}

// DiscoveryExclusionStore — v0.89.37 (#656 Stream 54, #531 slice 2
// chunk 4) — slim slice of ApplicationStore the
// HandleAWSRecommendationExclude handler reads + writes for the new
// iac_recommendation_verdicts table. Stated as a small interface so
// tests can substitute a fake without spinning up the SQLite layer.
type DiscoveryExclusionStore interface {
	SetRecommendationExclusion(
		ctx context.Context,
		rec types.ExcludedRecommendation,
		excluded bool,
	) (prevExcluded bool, err error)

	// ListExcludedRecommendations — v0.89.40 (#660 Stream 58, #531
	// slice 2 chunk 5 follow-on). Read surface the new
	// HandleAWSRecommendationListExcluded route consults to hydrate the
	// UI's excludedSet on tab mount. Matches the storage method's
	// signature one-for-one (the application store satisfies this
	// interface directly; tests substitute a fake).
	//
	// Empty (connectionID, accountID, region) tuples return no rows;
	// limit<=0 falls through to a small default in the implementation.
	ListExcludedRecommendations(
		ctx context.Context,
		connectionID, accountID, region string,
		limit int,
	) ([]types.ExcludedRecommendation, error)
}

// DiscoveryAcceptedRecommendationsAssembler is the slim contract
// HandleAWSGenerateRecommendations calls to populate the verdict
// few-shot block. The wiring-layer adapter resolves the
// (account_id, region) scope to a per-connection lookup internally —
// the handler doesn't need to know how the connection maps to the
// AWS scope. Production wires an adapter that delegates to
// *proposer.DiscoveryBridge; tests substitute a stub.
//
// v0.89.28 (#643 slice 1) → v0.89.36 (#655 Stream 53, #531 slice 2
// chunk 3): AssembleForDiscoveryScope still returns the v0.89.28
// accepted-only examples + URL list for backward compat with stubs
// and SIEM expectations. New surface AssembleVerdictBlock returns
// the fully-rendered verdict prompt stanza (including the
// [CLOSED_NOT_MERGED] negative-signal lines) plus the unioned URL
// list across both states. Both methods short-circuit identically on
// cold start / opt-out / recency-window empty.
type DiscoveryAcceptedRecommendationsAssembler interface {
	AssembleForDiscoveryScope(
		ctx context.Context,
		accountID, region string,
	) ([]ai.AcceptedRecommendationExample, []string, error)

	// AssembleVerdictBlock is the v0.89.36 (#655 Stream 53) entry
	// point. Returns:
	//   - verdictBlock: the fully-rendered prompt stanza produced by
	//     verdictprompt.Render, or "" on cold start / opt-out /
	//     recency-window empty.
	//   - urls: the unioned PR URL list across BOTH the merged and
	//     closed_not_merged buckets, in selection order (rejected
	//     first, then approved). Empty on cold start.
	AssembleVerdictBlock(
		ctx context.Context,
		accountID, region string,
	) (verdictBlock string, urls []string, err error)

	// AssembleVerdictBlockWithByState is the v0.89.37 (#657 Stream 55,
	// #531 slice 2 chunk 6) extension. Same return as
	// AssembleVerdictBlock for verdictBlock + urls, plus a per-state
	// bucket map that powers the audit payload's new
	// verdict_examples_used_by_state field. The map keys are the
	// discovery-surface state strings ("merged", "closed_not_merged",
	// "operator_excluded"); the union of bucket values equals urls
	// (modulo selection ordering). On cold start every bucket is
	// empty (nil map). Implementations may add new state keys without
	// a contract break; humanizer + SIEM consumers tolerate unknown
	// keys per spec §8 (c).
	AssembleVerdictBlockWithByState(
		ctx context.Context,
		accountID, region string,
	) (verdictBlock string, urls []string, urlsByState map[string][]string, err error)
}

// AWSCredMarshaller turns the wizard-supplied AWSCredentials into the
// ciphertext + nonce pair StoreConnection expects. Production wires
// the credstore-key version (see WithCredstoreKey); tests substitute a
// pass-through that returns the JSON plaintext as "ciphertext" so the
// Save handler can be exercised without holding the encryption key.
type AWSCredMarshaller func(creds credstore.AWSCredentials) (ciphertext, nonce []byte, err error)

// NewDiscoveryHandlers builds a DiscoveryHandlers wired with the
// default production AWS validator factory. credStore may be nil at
// construction — slice 1's validate endpoint doesn't read or write
// it; the Save endpoint requires it (and 500s if nil at request time).
// logger must be non-nil.
func NewDiscoveryHandlers(credStore credstore.Store, logger *zap.Logger) *DiscoveryHandlers {
	return &DiscoveryHandlers{
		credStore:       credStore,
		awsValidatorFor: defaultAWSValidatorFactory,
		logger:          logger,
		recJobs:         defaultRecommendationJobStore,
	}
}

// WithAWSValidatorFactory overrides the AWS validator builder. Used
// by tests to swap in a mock without touching the SDK; production
// callers don't need to call it. Returns the receiver so it can be
// chained off NewDiscoveryHandlers in test setup.
func (h *DiscoveryHandlers) WithAWSValidatorFactory(f AWSValidatorFactory) *DiscoveryHandlers {
	h.awsValidatorFor = f
	return h
}

// WithAuditService wires the audit recorder used by the Save handler.
// Optional — a nil auditService is treated as "no audit emission" so
// the test_server.go path (which never wires audit for discovery)
// stays compiling. Production wires the same auditService the
// orchestrator uses for the rest of Squadron's audit timeline.
func (h *DiscoveryHandlers) WithAuditService(a services.AuditService) *DiscoveryHandlers {
	h.auditService = a
	return h
}

// WithCredstoreKey wires the encryption key the Save handler uses to
// seal AWSCredentials. Production callers pass the key the active
// credstore.Store was opened with — that's the only way the later scan
// engine can decrypt the row. Tests use WithCredMarshaller below to
// substitute a pass-through.
//
// Wiring the key here also installs the production AWSScannerFactory:
// the same key the Save handler uses to encrypt credentials is what
// the scan handler needs to decrypt them back when the operator
// triggers a run. Keeping both behind the same setter avoids the
// half-wired posture where Save persists rows the scan handler can't
// read.
func (h *DiscoveryHandlers) WithCredstoreKey(key *credstore.Key) *DiscoveryHandlers {
	h.awsCredMarshaller = func(creds credstore.AWSCredentials) ([]byte, []byte, error) {
		return credstore.MarshalAWSCredentials(creds, key)
	}
	h.awsScannerFor = defaultAWSScannerFactory(key)
	return h
}

// WithCredMarshaller overrides the AWSCredMarshaller. Tests use this
// to inject a pass-through that records the cleartext creds without
// invoking the AEAD; production callers use WithCredstoreKey.
func (h *DiscoveryHandlers) WithCredMarshaller(m AWSCredMarshaller) *DiscoveryHandlers {
	h.awsCredMarshaller = m
	return h
}

// WithAWSScannerFactory overrides the AWS scanner factory. Tests use
// this to inject a stub scanner that returns a pre-canned Result
// without ever calling AWS; production callers either let
// WithCredstoreKey install the default or, in test_server.go's
// no-discovery posture, leave it nil so the scan endpoint 500s with a
// clear "scanner not wired" humanized error.
func (h *DiscoveryHandlers) WithAWSScannerFactory(f AWSScannerFactory) *DiscoveryHandlers {
	h.awsScannerFor = f
	return h
}

// WithAIProposer wires the v0.85 Stream 2F discovery-side AI proposer.
// Production wires *ai.Service (which satisfies the interface); tests
// substitute a mock that returns a pre-canned ProposalResult. A nil
// proposer leaves HandleAWSGenerateRecommendations returning 503 with
// a clear "AI assist not configured" message; the rest of the
// discovery surface stays unaffected.
func (h *DiscoveryHandlers) WithAIProposer(p DiscoveryAIProposer) *DiscoveryHandlers {
	h.aiProposer = p
	return h
}

// WithAcceptedRecommendationsAssembler wires the v0.89.28 (#643 slice 1)
// accepted-recommendations few-shot assembler. nil is fine — the
// HandleAWSGenerateRecommendations path treats it as cold-start.
func (h *DiscoveryHandlers) WithAcceptedRecommendationsAssembler(a DiscoveryAcceptedRecommendationsAssembler) *DiscoveryHandlers {
	h.acceptedAssembler = a
	return h
}

// WithExclusionStore — v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4)
// — wires the operator-set exclusion store the
// HandleAWSRecommendationExclude handler consults. Production wires
// the application store directly (which satisfies the interface);
// tests substitute a fake. A nil store leaves the exclude route
// returning 503 with a clear "not wired" message.
func (h *DiscoveryHandlers) WithExclusionStore(s DiscoveryExclusionStore) *DiscoveryHandlers {
	h.exclusionStore = s
	return h
}

// WithChecksClient — v0.89.44 (#665 Stream 63, slice 1 chunk 4) —
// wires the Checks API client used by the chunk-4 PATCH-to-neutral
// follow-up inside HandleAWSRecommendationExclude. Nil keeps the
// follow-up dormant: the existing discovery_recommendation.excluded
// audit emit completes normally with no check-run side-effects (the
// open PR's check run on GitHub stays in_progress until the PR
// merges or closes, which the chunk-3 webhook handler then PATCHes).
// See design doc §5 fail-open posture and §10 contract item 7.
func (h *DiscoveryHandlers) WithChecksClient(c ChecksAPI) *DiscoveryHandlers {
	h.checksClient = c
	return h
}

// WithCheckRunStore — v0.89.44 (#665 Stream 63, slice 1 chunk 4) —
// wires the application-store surface the chunk-4 follow-up consults
// to look up the in-flight check-run ref for a recommendation_id
// before PATCHing it to neutral. The application store satisfies the
// slim CheckRunStore interface directly; tests substitute a recording
// fake. A nil store leaves the lookup short-circuited (the audit
// emit + 200 still complete normally).
func (h *DiscoveryHandlers) WithCheckRunStore(s CheckRunStore) *DiscoveryHandlers {
	h.checkRunStore = s
	return h
}

// WithChecksPAT — v0.89.44 (#665 Stream 63, slice 1 chunk 4) — wires
// the PAT the chunk-4 follow-up uses to authenticate to GitHub's
// Checks API when PATCHing a check run to neutral on operator
// exclude. Per design doc §3 option A this is the same PAT-backed
// model the rest of slice 1 ships on; production wires the same PAT
// the IaC connection uses to open PRs, surfaced here so the discovery
// handler — which does not unseal the IaC credstore — can authenticate
// the follow-up without a per-call decryption hop. An empty PAT keeps
// the chunk-4 follow-up dormant (matches the nil-client posture).
//
// SECURITY: the PAT is stored on the handler struct for the
// deployment lifetime. The build-edition layer in cmd/all-in-one is
// responsible for sourcing it from the same secret-manager pipeline
// the iacgithub.PATClient already uses. Tests pass a fixture string.
func (h *DiscoveryHandlers) WithChecksPAT(pat string) *DiscoveryHandlers {
	h.checksPAT = pat
	return h
}

// WithTraceIndex — v0.89.77 (#708 Stream 106, Trace integration
// slice 1 chunk 4) — wires the traceindex lookup used to annotate
// scan responses with per-resource last_seen_at. Nil leaves the
// scan response un-annotated. Production wires the same Index
// chunk 3 wired into the Discovery dashboard; tests substitute a
// stub returning canned timestamps.
func (h *DiscoveryHandlers) WithTraceIndex(idx TraceIndexLookup) *DiscoveryHandlers {
	h.traceIndex = idx
	return h
}

// WithColdStartObservationStore — Cold-start latency analysis slice 1
// chunk 3 (v0.89.115, #753 Stream 151) — wires the storage adapter the
// AnnotateServerlessWithColdStart pass uses to populate the per-Lambda
// cold_start_p95_ms + cold_start_exceeds_threshold fields on the scan
// response. Nil leaves the fields unannotated (rendered as "—" in
// the UI). Production wires the chunk-1 *sqlite.Storage; tests
// substitute a fake returning canned ColdStartObservationRows. The
// thresholds adapter pins the four substrate constants (24h current,
// 168h baseline, 1.5x ratio, 500ms floor) — production threads the
// staticColdStartDetectionConstants from the AWS substrate so the
// values stay single-sourced.
func (h *DiscoveryHandlers) WithColdStartObservationStore(store ColdStartObservationReader, thresholds ColdStartAnnotationThresholds) *DiscoveryHandlers {
	h.coldStartStore = store
	h.coldStartConstants = thresholds
	return h
}

// WithErrorRateObservationStore — Error rate correlation slice 1
// chunk 3 (v0.89.129, #769 Stream 167) — wires the storage adapter
// the AnnotateServerlessWithErrorRate pass uses to populate the
// per-Serverless current_error_rate +
// error_rate_exceeds_threshold fields. Nil leaves the fields
// unannotated ("—" in the UI). Production wires the chunk-1
// *sqlite.Storage; tests substitute a fake.
func (h *DiscoveryHandlers) WithErrorRateObservationStore(store ErrorRateObservationStore) *DiscoveryHandlers {
	h.errorRateStore = store
	return h
}

// WithSquadronHost — v0.89.44 (#665 Stream 63, slice 1 chunk 4) —
// configures the base URL the check-run summary's "View in Squadron"
// deep link targets. Empty value suppresses the link line rather
// than emitting a broken (/) href. Mirrors the IaCGitHubHandlers
// chunk-2 setter so the same SQUADRON_PUBLIC_HOST env var threads
// through both handlers.
func (h *DiscoveryHandlers) WithSquadronHost(host string) *DiscoveryHandlers {
	h.squadronHost = host
	return h
}

// validateHandlerTimeout caps the validate endpoint's total wall-clock
// budget. Defense in depth: the AWS SDK call path is already wrapped
// in a 5s credential-discovery context (client.go), but if a future
// regression re-introduces a slow path the HTTP handler must never
// hang beyond a known budget. 60s comfortably exceeds the SDK's
// happy-path round trip (sub-second for sts:AssumeRole on a healthy
// link) while still bounding the worst case.
const validateHandlerTimeout = 60 * time.Second

// scanHandlerTimeout caps the scan endpoint's total wall-clock budget.
// Scans can legitimately span minutes on a 50k+ resource account
// (DescribeInstances paginates 100 per call, plus a Lambda
// ListFunctions sweep per region). 5 minutes is the design doc's
// upper bound for slice 1; slice 3 introduces async scans where this
// budget shrinks to enqueue latency.
const scanHandlerTimeout = 5 * time.Minute

// awsValidateRequest is the JSON wire shape the wizard's
// test-before-commit step POSTs. Mirrors the design doc's
// "Validation endpoint" section.
//
// AccountID is optional — when present, the scanner records it on
// the result; when absent, the scanner derives it from
// sts:GetCallerIdentity. Slice 1's UI populates it from the operator's
// "AWS account ID" wizard step so the response is fully self-
// describing even on assume-role failure.
type awsValidateRequest struct {
	RoleARN    string   `json:"role_arn"`
	ExternalID string   `json:"external_id"`
	Regions    []string `json:"regions"`
	AccountID  string   `json:"account_id"`
}

// awsValidateResponse is the wire shape the wizard renders into the
// "what just happened" panel. It mirrors scanner.ValidationResult but
// with json tags + a top-level errors[] convenience field so the UI
// can render assume-role and preflight failures in one list without
// walking the typed structs.
type awsValidateResponse struct {
	AssumeRoleOK  bool                      `json:"assume_role_ok"`
	AssumeRoleErr *scanner.HumanizedError   `json:"assume_role_err,omitempty"`
	Preflight     []awsValidatePreflightRow `json:"preflight"`
	Errors        []scanner.HumanizedError  `json:"errors,omitempty"`
}

type awsValidatePreflightRow struct {
	Service     string                  `json:"service"`
	OK          bool                    `json:"ok"`
	SampleCount int                     `json:"sample_count"`
	Err         *scanner.HumanizedError `json:"err,omitempty"`
}

// HandleAWSValidate — POST /api/v1/discovery/aws/validate.
//
// Per the design doc, this handler:
//   - validates the request body shape (400 on missing role_arn or
//     external_id, with a humanized error pointing at the offending
//     wizard step);
//   - constructs a transient CloudConnection — NOT persisted, NOT
//     written to credstore;
//   - hands the transient connection to a DiscoveryValidator and
//     returns the typed result as JSON.
//
// Zero records are created — that's the design contract. The (later)
// Save endpoint is where audit events land.
func (h *DiscoveryHandlers) HandleAWSValidate(c *gin.Context) {
	var req awsValidateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message:       "Request body could not be parsed as JSON. Check the wizard's payload shape.",
			SuggestedStep: "validate",
		}})
		return
	}
	if strings.TrimSpace(req.RoleARN) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingRoleARN",
			Message:       "Role ARN is required. Paste the value from Step 3 of the AWS wizard.",
			SuggestedStep: "role-arn",
		}})
		return
	}
	if strings.TrimSpace(req.ExternalID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingExternalID",
			Message:       "External ID is required — the trust policy is unsafe without it. Copy the value Squadron generated in Step 2.",
			SuggestedStep: "trust-policy",
		}})
		return
	}

	conn := &credstore.CloudConnection{
		AccountID:      req.AccountID,
		Provider:       credstore.ProviderAWS,
		ConnectionType: credstore.ConnectionAPIDiscovered,
		Regions:        req.Regions,
	}

	if h.awsValidatorFor == nil {
		// Belt-and-braces: NewDiscoveryHandlers always sets a
		// factory, but a future caller might construct the handler
		// by struct literal and forget. Surface that as a 500 so the
		// failure is visible in tests rather than silently 200-ing.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "AWS validator factory not wired"})
		return
	}
	validator := h.awsValidatorFor(credstore.AWSCredentials{
		RoleARN:    req.RoleARN,
		ExternalID: req.ExternalID,
	}, req.AccountID)

	// Defense in depth: even though the AWS SDK call path in
	// internal/discovery/aws/client.go already wraps credential
	// discovery in a 5s budget, the HTTP handler enforces its own
	// 60s ceiling so a future regression on the slow path can't
	// resurrect the v0.85.0 30-second-hang bug. The wizard's
	// "Validate connection" spinner stays bounded by a value the
	// operator can recognize as "something went wrong" rather than
	// "the page is still loading".
	ctx, cancel := context.WithTimeout(c.Request.Context(), validateHandlerTimeout)
	defer cancel()
	vr, err := validator.Validate(ctx, conn)
	if err != nil {
		// scanner.Validate is documented as never returning an
		// error on the AWS path — all failures land in AssumeRoleErr
		// or per-preflight Err. A non-nil error from a future
		// implementation deserves a 500, not a leaked 200.
		if h.logger != nil {
			h.logger.Warn("aws validate: scanner returned error", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "ValidatorInternal",
			Message:       "Squadron's validator raised an internal error. Retry; if the failure persists, file an issue.",
			SuggestedStep: "validate",
		}})
		return
	}

	c.JSON(http.StatusOK, marshalValidationResult(vr))
}

// awsSaveConnectionRequest is the JSON wire shape the wizard's
// final Save step POSTs. Mirrors the design doc's "Connector workflow
// design > Architecture" section: every field the substrate row
// needs lands here, including the freshly-generated ExternalId from
// the wizard's Trust policy step.
type awsSaveConnectionRequest struct {
	AccountID   string   `json:"account_id"`
	RoleARN     string   `json:"role_arn"`
	ExternalID  string   `json:"external_id"`
	DisplayName string   `json:"display_name"`
	Regions     []string `json:"regions"`
}

// awsSaveConnectionResponse is the wire shape returned on a successful
// Save. The connection_id today is just the AccountID (the substrate's
// primary key); a future slice may add a server-side row UUID, at
// which point the field decouples — but the wire name stays stable.
type awsSaveConnectionResponse struct {
	ConnectionID string `json:"connection_id"`
	Status       string `json:"status"`
}

// HandleAWSSaveConnection — POST /api/v1/discovery/aws/connections.
//
// Per the design doc's "Connector workflow design > Architecture"
// section, this handler:
//   - validates the request body shape;
//   - re-runs scanner.Validate one last time (the operator may have
//     edited the role between the wizard's Validate step and Save —
//     better to fail here than persist a broken connection);
//   - marshals + encrypts the AWSCredentials via the credstore key;
//   - persists the CloudConnection through credstore.Store;
//   - emits a discovery.aws.connection_created audit event with the
//     account ID, role ARN, and regions — NEVER the ExternalId.
//
// Returns 201 with {connection_id, status} on success. 400 on bad
// request or pre-persist validation failure (no row is written and no
// audit event fires). 500 if the credstore write itself fails (the
// audit event still does not fire — the operator's UI should retry).
func (h *DiscoveryHandlers) HandleAWSSaveConnection(c *gin.Context) {
	var req awsSaveConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message:       "Request body could not be parsed as JSON. Check the wizard's payload shape.",
			SuggestedStep: "save",
		}})
		return
	}
	if strings.TrimSpace(req.AccountID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingAccountID",
			Message:       "Account ID is required. Paste the value from Step 1 of the AWS wizard.",
			SuggestedStep: "account-id",
		}})
		return
	}
	if strings.TrimSpace(req.RoleARN) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingRoleARN",
			Message:       "Role ARN is required. Paste the value from Step 3 of the AWS wizard.",
			SuggestedStep: "role-arn",
		}})
		return
	}
	if strings.TrimSpace(req.ExternalID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingExternalID",
			Message:       "External ID is required — the trust policy is unsafe without it. Copy the value Squadron generated in Step 2.",
			SuggestedStep: "trust-policy",
		}})
		return
	}
	if len(req.Regions) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingRegions",
			Message:       "At least one region is required. Slice 1 ships single-region; pick the region you want Squadron to scan.",
			SuggestedStep: "validate",
		}})
		return
	}

	if h.credStore == nil {
		// The trampoline already 503s when credStore is nil. This is a
		// belt-and-braces guard for direct-struct construction.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreNotWired",
			Message:       "Squadron's credential substrate isn't configured. Restart the server with SQUADRON_SECRETS_KEY set.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.awsValidatorFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "AWS validator factory not wired"})
		return
	}
	if h.awsCredMarshaller == nil {
		// Same posture as credStore — production callers wire via
		// WithCredstoreKey; the trampoline enforces this.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredKeyNotWired",
			Message:       "Squadron's credential encryption key isn't configured. The Save flow cannot persist without it.",
			SuggestedStep: "save",
		}})
		return
	}

	// Re-run the assume-role probe one last time. The operator may
	// have edited the IAM role between the wizard's Validate step and
	// Save (e.g. trimmed the trust-policy ExternalId condition by
	// mistake) — better to surface the failure now than write a row
	// that will fail at first scan.
	transientConn := &credstore.CloudConnection{
		AccountID:      req.AccountID,
		Provider:       credstore.ProviderAWS,
		ConnectionType: credstore.ConnectionAPIDiscovered,
		Regions:        req.Regions,
	}
	validator := h.awsValidatorFor(credstore.AWSCredentials{
		RoleARN:    req.RoleARN,
		ExternalID: req.ExternalID,
	}, req.AccountID)
	// Same 60s ceiling as HandleAWSValidate — the re-validate step
	// runs the same code path and must never resurrect the v0.85.0
	// 30-second-hang bug.
	saveCtx, saveCancel := context.WithTimeout(c.Request.Context(), validateHandlerTimeout)
	defer saveCancel()
	vr, err := validator.Validate(saveCtx, transientConn)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("aws save: pre-persist validator error", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "ValidatorInternal",
			Message:       "Squadron's validator raised an internal error. Retry; if the failure persists, file an issue.",
			SuggestedStep: "validate",
		}})
		return
	}
	if !vr.AssumeRoleOK {
		// AssumeRoleErr is the humanized payload the wizard renders.
		// 400 (not 500) — this is operator-recoverable.
		out := marshalValidationResult(vr)
		c.JSON(http.StatusBadRequest, gin.H{"error": vr.AssumeRoleErr, "validation": out})
		return
	}

	// Encrypt the credentials. The substrate stores ciphertext-only.
	ciphertext, nonce, err := h.awsCredMarshaller(credstore.AWSCredentials{
		RoleARN:    req.RoleARN,
		ExternalID: req.ExternalID,
	})
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws save: cred marshal failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredentialEncryptFailed",
			Message:       "Squadron could not encrypt the role credentials. Verify SQUADRON_SECRETS_KEY is set and re-run Save.",
			SuggestedStep: "save",
		}})
		return
	}

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = req.AccountID
	}
	conn := credstore.CloudConnection{
		AccountID:        req.AccountID,
		Provider:         credstore.ProviderAWS,
		ConnectionType:   credstore.ConnectionAPIDiscovered,
		DisplayName:      displayName,
		Regions:          req.Regions,
		Credentials:      ciphertext,
		CredentialsNonce: nonce,
	}
	if err := h.credStore.StoreConnection(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("aws save: credstore write failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreWriteFailed",
			Message:       "Squadron could not persist the connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	// Audit event. ExternalId is deliberately NOT in the payload —
	// the design doc treats it as a per-deployment secret and the
	// substrate's audit invariants forbid leaking it.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      "system",
			EventType:  "discovery.aws.connection_created",
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   req.AccountID,
			Action:     "created",
			Payload: map[string]any{
				"account_id":   req.AccountID,
				"role_arn":     req.RoleARN,
				"regions":      req.Regions,
				"display_name": displayName,
				"recorded_at":  time.Now().UTC(),
			},
		})
	}

	c.JSON(http.StatusCreated, awsSaveConnectionResponse{
		ConnectionID: req.AccountID,
		Status:       "connected",
	})
}

// awsConnectionRow is the redacted view of a stored CloudConnection
// the list endpoint returns. ONLY display fields land here — never the
// role ARN, never the encrypted credentials blob, never the
// CredentialsNonce, never the ExternalId.
//
// The substrate's CloudConnection struct already json:"-" the
// Credentials + CredentialsNonce bytes, but the role ARN ciphertext
// would still slip through if we marshaled the whole row. Defining a
// purpose-built row type makes the redaction explicit and survives a
// future addition of new fields to CloudConnection without leaking
// them by default.
//
// The connection_id field is equal to account_id today — the
// substrate's CloudConnection has no separate UUID. Surfacing it as a
// named field lets the UI construct /connections/:id/scan URLs without
// inferring that account_id IS the connection id, and lets a future
// substrate change to UUIDs not require a wire-shape break.
type awsConnectionRow struct {
	ConnectionID string    `json:"connection_id"`
	AccountID    string    `json:"account_id"`
	DisplayName  string    `json:"display_name"`
	Regions      []string  `json:"regions"`
	CreatedAt    time.Time `json:"created_at"`
}

// awsListConnectionsResponse is the wire shape the Account tab fetches.
// Empty array (NOT null) when no connections exist — the UI's empty
// state keys off `connections.length === 0`, not on a missing field.
type awsListConnectionsResponse struct {
	Connections []awsConnectionRow `json:"connections"`
}

// HandleAWSListConnections — GET /api/v1/discovery/aws/connections.
//
// Returns every stored AWS connection's display fields. Operators see
// "this account is connected" plus the display name, regions, and
// creation time; they cannot read back the role ARN, the external_id,
// or any encrypted credential material. The Credentials /
// CredentialsNonce bytes never leave this handler — they're not even
// surfaced into the response struct.
//
// Empty store returns {"connections": []} with 200 (NOT 404). The
// Account tab treats both as a "no accounts yet" empty state.
func (h *DiscoveryHandlers) HandleAWSListConnections(c *gin.Context) {
	if h.credStore == nil {
		// Belt-and-braces: the trampoline already 503s. This keeps the
		// handler safe to call from struct-literal construction in
		// tests that forget to wire a store.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreNotWired",
			Message:       "Squadron's credential substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}

	conns, err := h.credStore.ListConnections(c.Request.Context(), credstore.ListFilter{
		Provider: credstore.ProviderAWS,
	})
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws list connections: credstore read failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreReadFailed",
			Message:       "Squadron could not read the connection list. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	// Always emit an array — never null — so the UI's empty-state
	// branch is a single .length check.
	rows := make([]awsConnectionRow, 0, len(conns))
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		rows = append(rows, awsConnectionRow{
			ConnectionID: conn.AccountID,
			AccountID:    conn.AccountID,
			DisplayName:  conn.DisplayName,
			Regions:      conn.Regions,
			CreatedAt:    conn.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, awsListConnectionsResponse{Connections: rows})
}

// awsRunScanRequest is the optional body for the scan endpoint. When
// Regions is empty (or the body is absent altogether), the scanner
// falls back to the connection's stored Regions list. Slice 1's UI
// always populates the field to make the per-scan region selection
// explicit; the empty-body path keeps the endpoint curl-friendly.
type awsRunScanRequest struct {
	Regions []string `json:"regions"`

	// Tiers — serverless-tier slice 1 chunk 1 (v0.89.90, #721 Stream
	// 119). Optional explicit tier list. When non-empty, narrows the
	// per-region walk to the named tiers; when empty (or absent),
	// the scanner walks every tier in the default set. The opt-in
	// posture preserves backward-compat — existing callers who omit
	// the field get the full surface, just as they did before
	// slice 1.
	//
	// Valid values: "compute" / "database" / "kubernetes" /
	// "serverless". Unknown values are silently ignored (operators
	// who hand-rolled a curl with a typo get the safe default
	// rather than a 400).
	//
	// Today the AWS scanner ignores Tiers — the per-region walk
	// always covers every tier. Chunk 5 wires Tiers through to the
	// scanner once the UI surfaces a per-tier filter; chunk 1's
	// contribution is the validated parse path so the wire shape
	// is stable as soon as serverless lands.
	//
	// See docs/proposals/serverless-tier-slice1.md §6.1.
	Tiers []string `json:"tiers,omitempty"`
}

// TierCompute / TierDatabase / TierKubernetes / TierServerless /
// TierOrchestration / TierEventSource are the tier identifiers the scan
// endpoint's Tiers field accepts. DefaultScanTiers is the implicit value
// when the field is empty — the full surface, matching the design doc
// §6.1: the event-source-tier arc (v0.89.100, #734 Stream 132) widens the
// default from [compute, database, kubernetes, serverless, orchestration]
// to [compute, database, kubernetes, serverless, orchestration,
// event_source].
const (
	TierCompute       = "compute"
	TierDatabase      = "database"
	TierKubernetes    = "kubernetes"
	TierServerless    = "serverless"
	TierOrchestration = "orchestration"
	TierEventSource   = "event_source"
)

// DefaultScanTiers is the default tier list applied when a scan
// request omits the Tiers field. Event-source-tier slice 1 chunk 1
// (v0.89.100, #734 Stream 132) widens the default from
// [compute, database, kubernetes, serverless, orchestration] to include
// event_source. Operators who passed an explicit list keep their old
// behavior; default callers get the wider surface.
var DefaultScanTiers = []string{
	TierCompute, TierDatabase, TierKubernetes, TierServerless, TierOrchestration, TierEventSource,
}

// parseTiersOrDefault normalizes the request's Tiers field into the
// canonical tier list. Empty input falls back to DefaultScanTiers;
// unknown tier strings are silently dropped (an operator's typo
// shouldn't 400 the scan). Returns a defensively-copied slice so the
// caller can mutate without leaking back to the request.
//
// Used by HandleAWSRunScan + the future GCP / Azure / OCI scan
// handlers to apply consistent tier parsing across providers.
func parseTiersOrDefault(in []string) []string {
	if len(in) == 0 {
		out := make([]string, len(DefaultScanTiers))
		copy(out, DefaultScanTiers)
		return out
	}
	out := make([]string, 0, len(in))
	for _, t := range in {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case TierCompute, TierDatabase, TierKubernetes, TierServerless, TierOrchestration, TierEventSource:
			out = append(out, strings.ToLower(strings.TrimSpace(t)))
		}
	}
	if len(out) == 0 {
		// Every tier was unrecognized — fall back to the default
		// rather than scan nothing. Matches the "intent is scan
		// whatever's configured" curl-friendliness posture.
		out = append(out, DefaultScanTiers...)
	}
	return out
}

// tierListContains is a tiny helper used by the orchestration-tier
// dispatch path in runAWSScan: returns true when the canonical tier
// list contains the supplied tier. Kept as a free function (rather
// than a method) so the per-tier dispatch sites read naturally —
// "if tierListContains(tiers, TierOrchestration)" pairs with
// parseTiersOrDefault. Slice 1 chunk 1 of the orchestration-tier arc
// (v0.89.95, #728 Stream 126).
func tierListContains(tiers []string, want string) bool {
	for _, t := range tiers {
		if t == want {
			return true
		}
	}
	return false
}

// awsScanResponse is the snake_case wire shape the React Inventory tab
// consumes. scanner.Result is defined without JSON tags (the scanner
// package is provider-agnostic and the handler owns the wire contract),
// so we walk it once into a tagged struct rather than emit the Go
// field names verbatim.
type awsScanResponse struct {
	ScanID          string                   `json:"scan_id"`
	ScanStartedAt   time.Time                `json:"scan_started_at"`
	ScanCompletedAt time.Time                `json:"scan_completed_at"`
	AccountID       string                   `json:"account_id"`
	Provider        string                   `json:"provider"`
	Regions         []string                 `json:"regions"`
	Compute         []awsComputeInstanceRow  `json:"compute"`
	Functions       []awsFunctionRuntimeRow  `json:"functions"`
	Databases       []awsDatabaseInstanceRow `json:"databases"`
	// ObjectStores + LoadBalancers join the wire shape in slice 3a
	// (v0.88.0). Always emitted as arrays (never null) so the UI's
	// empty-state branch is a single `.length === 0` check —
	// matching the existing Compute / Functions / Databases
	// posture.
	ObjectStores  []awsObjectStoreRow  `json:"object_stores"`
	LoadBalancers []awsLoadBalancerRow `json:"load_balancers"`
	// Clusters joins the wire shape in slice 3b (v0.89.0). Same
	// non-null posture as the other category arrays.
	Clusters []awsClusterRow `json:"clusters"`
	// DynamoDBTables joins the wire shape in slice 4 (v0.89.6).
	// Same non-null posture as the other category arrays.
	DynamoDBTables []awsDynamoDBTableRow `json:"dynamodb_tables"`
	// ECSClusters joins the wire shape in slice 5 (v0.89.10). Same
	// non-null posture as the other category arrays.
	ECSClusters []awsECSClusterRow `json:"ecs_clusters"`
	// Serverless joins the wire shape in serverless-tier slice 1
	// chunk 1 (v0.89.90, #721 Stream 119). Same non-null posture as
	// the other category arrays. Carries the cross-cloud detection
	// shape (provider / surface / two-axis booleans + LastSeenAt)
	// alongside a per-surface Detail bag the per-cloud Inventory
	// tab renders.
	Serverless []awsServerlessRow `json:"serverless"`
	// Orchestrations joins the wire shape in orchestration-tier
	// slice 1 chunk 1 (v0.89.95, #728 Stream 126). Same non-null
	// posture as the other category arrays. Carries the cross-cloud
	// detection shape (provider / surface / two-axis booleans +
	// LastSeenAt) alongside a per-surface Detail bag the per-cloud
	// Inventory tab renders. Slice 1 chunk 1 only populates AWS
	// Step Functions rows; chunk 2 adds GCP Workflows and chunk 3
	// adds Azure Logic Apps. Empty for OCI throughout slice 1.
	Orchestrations []awsOrchestrationRow `json:"orchestrations"`
	// EventSources joins the wire shape in event-source-tier slice 1
	// chunk 1 (v0.89.100, #734 Stream 132). Same non-null posture as
	// the other category arrays. Carries the cross-cloud detection
	// shape (provider / surface / two-axis booleans + LastSeenAt)
	// alongside a per-surface Detail bag the per-cloud Inventory tab
	// renders. Slice 1 chunk 1 only populates AWS EventBridge rows;
	// chunk 2 adds GCP Pub/Sub, chunk 3 adds Azure Service Bus,
	// chunk 4 adds OCI Streaming.
	EventSources        []eventSourceRow `json:"event_sources"`
	InstrumentedCount   int              `json:"instrumented_count"`
	UninstrumentedCount int              `json:"uninstrumented_count"`
	Partial             bool             `json:"partial"`
	PartialReason       string           `json:"partial_reason,omitempty"`
	// FailedServices lists the service/tier identifiers whose walk
	// produced a non-fatal error this scan (e.g. "event_source",
	// "orchestration", "ec2"). Mirrors PartialReason in structured
	// form so the UI can render which tiers were degraded/denied
	// instead of showing an empty inventory as if it were complete.
	FailedServices []string `json:"failed_services,omitempty"`
}

type awsComputeInstanceRow struct {
	ResourceID   string            `json:"resource_id"`
	InstanceType string            `json:"instance_type"`
	Tags         map[string]string `json:"tags"`
	HasOTel      bool              `json:"has_otel"`
	OSFamily     string            `json:"os_family"`
	Region       string            `json:"region"`
	// LastSeenAt — v0.89.77 trace integration slice 1 chunk 4. ISO
	// timestamp of the most recent span the receiver observed for
	// this resource. Omitted on the wire when nil (the row renders
	// "never" in the UI).
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

type awsFunctionRuntimeRow struct {
	ResourceID   string `json:"resource_id"`
	Name         string `json:"name"`
	Runtime      string `json:"runtime"`
	HasOTelLayer bool   `json:"has_otel_layer"`
	Region       string `json:"region"`
}

// awsDatabaseInstanceRow is the snake_case wire shape for one RDS row.
// Mirrors scanner.DatabaseInstanceSnapshot — the two observability
// lever flags surface as separate booleans so the Inventory tab can
// render them as independent badge columns, matching the proposer
// prompt's "treat PI + EM as independent levers" framing.
//
// Database tier slice 2 (v0.89.66, #695 Stream 93) — extended with
// Provider + the three per-cloud observability axis flags so the
// generate-recommendations request body can carry GCP / Azure / OCI
// database rows through the same wire shape. The new fields use
// omitempty so AWS-only request bodies emitted before this release
// deserialize unchanged.
type awsDatabaseInstanceRow struct {
	ResourceID                 string            `json:"resource_id"`
	Engine                     string            `json:"engine"`
	EngineVersion              string            `json:"engine_version"`
	InstanceClass              string            `json:"instance_class"`
	PerformanceInsightsEnabled bool              `json:"performance_insights_enabled"`
	EnhancedMonitoringEnabled  bool              `json:"enhanced_monitoring_enabled"`
	Region                     string            `json:"region"`
	Tags                       map[string]string `json:"tags"`

	Provider                  string `json:"provider,omitempty"`
	QueryInsightsEnabled      bool   `json:"query_insights_enabled,omitempty"`
	SQLInsightsDiagEnabled    bool   `json:"sql_insights_diag_enabled,omitempty"`
	DatabaseManagementEnabled bool   `json:"database_management_enabled,omitempty"`

	// LastSeenAt — v0.89.77 trace integration slice 1 chunk 4. See
	// awsComputeInstanceRow.LastSeenAt godoc.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

// awsObjectStoreRow is the snake_case wire shape for one S3 row.
// Slice 3a (v0.88.0). Mirrors scanner.ObjectStoreSnapshot — the
// single instrumented-rule axis (server_access_logging_enabled)
// surfaces as a boolean; request_metrics_enabled is the
// informational badge column the Inventory tab renders alongside it.
type awsObjectStoreRow struct {
	ResourceID                 string            `json:"resource_id"`
	Region                     string            `json:"region"`
	ServerAccessLoggingEnabled bool              `json:"server_access_logging_enabled"`
	RequestMetricsEnabled      bool              `json:"request_metrics_enabled"`
	Tags                       map[string]string `json:"tags"`
}

// awsLoadBalancerRow is the snake_case wire shape for one ALB / NLB /
// GWLB row. Slice 3a (v0.88.0). Mirrors
// scanner.LoadBalancerSnapshot — the single instrumented-rule axis
// (access_logs_enabled) surfaces as a boolean alongside the
// access_logs_s3_bucket target so the Inventory tab can render the
// cross-reference to the operator's S3 inventory in a single column.
type awsLoadBalancerRow struct {
	ResourceID         string            `json:"resource_id"`
	Name               string            `json:"name"`
	Type               string            `json:"type"`
	Scheme             string            `json:"scheme"`
	AccessLogsEnabled  bool              `json:"access_logs_enabled"`
	AccessLogsS3Bucket string            `json:"access_logs_s3_bucket,omitempty"`
	Region             string            `json:"region"`
	Tags               map[string]string `json:"tags"`
}

// awsClusterRow is the snake_case wire shape for one EKS / GKE / AKS
// cluster row. Slice 3b (v0.89.0). Mirrors scanner.ClusterSnapshot —
// the composite instrumented-rule axes (control_plane_logging +
// addons[*].name+status) surface as their own fields so the
// Inventory tab renders both axes as independent badge groups,
// matching the proposer prompt's "BOTH must hold" framing.
type awsClusterRow struct {
	ResourceID          string               `json:"resource_id"`
	Name                string               `json:"name"`
	KubernetesVersion   string               `json:"kubernetes_version"`
	Status              string               `json:"status"`
	ControlPlaneLogging []string             `json:"control_plane_logging"`
	Addons              []awsClusterAddonRow `json:"addons"`
	NodegroupCount      int                  `json:"nodegroup_count"`
	FargateProfileCount int                  `json:"fargate_profile_count"`
	Region              string               `json:"region"`
	Tags                map[string]string    `json:"tags"`
	// LastSeenAt — v0.89.77 trace integration slice 1 chunk 4. See
	// awsComputeInstanceRow.LastSeenAt godoc.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

// awsClusterAddonRow is the snake_case wire shape for one EKS add-on
// row. Slice 3b (v0.89.0). Mirrors scanner.ClusterAddon — Name +
// Status drive the observability-detection rule the proposer reads.
type awsClusterAddonRow struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

// awsDynamoDBTableRow is the snake_case wire shape for one DynamoDB
// table row. Slice 4 (v0.89.6). Mirrors
// scanner.DynamoDBTableSnapshot — the single instrumented-rule axis
// (contributor_insights_status) surfaces as a string so the
// Inventory tab can render the four AWS API enum values plus the
// scanner's "UNKNOWN" fallback sentinel in a single badge column.
//
// SDK-side limitation (re-stated honestly): Squadron detects
// resource-side Contributor Insights here. Squadron does not detect
// SDK-side OpenTelemetry or X-Ray instrumentation in application
// code; tables whose DynamoDB SDK is OTel-wrapped on the client
// side render as uninstrumented in the inventory.
type awsDynamoDBTableRow struct {
	ResourceID                string            `json:"resource_id"`
	Name                      string            `json:"name"`
	Status                    string            `json:"status"`
	BillingMode               string            `json:"billing_mode,omitempty"`
	ContributorInsightsStatus string            `json:"contributor_insights_status"`
	Region                    string            `json:"region"`
	Tags                      map[string]string `json:"tags"`
}

// awsECSClusterRow is the snake_case wire shape for one ECS cluster
// row. Slice 5 (v0.89.10). Mirrors scanner.ECSClusterSnapshot — the
// single instrumented-rule axis (container_insights_status)
// surfaces as a string so the Inventory tab can render the three
// AWS-side enum values ("enabled" / "disabled" / "enhanced") plus
// the scanner's "UNKNOWN" fallback sentinel in a single badge
// column. Both Fargate and EC2 launch types are covered by the
// same per-cluster rule — the inventory row does not differentiate.
//
// Task-definition-level limitation (re-stated honestly): Squadron
// detects cluster-level CloudWatch Container Insights here.
// Squadron does not detect task-definition-level instrumentation
// — X-Ray daemon sidecars, ADOT collector sidecars, or FireLens
// log routing in your task definitions. If your task defs include
// those sidecars but the cluster does not have Container Insights
// enabled, Squadron will report the cluster as uninstrumented —
// this is a known limitation of cluster-level scanning.
type awsECSClusterRow struct {
	Name                              string            `json:"name"`
	ARN                               string            `json:"arn"`
	Status                            string            `json:"status"`
	ContainerInsightsStatus           string            `json:"container_insights_status"`
	RegisteredContainerInstancesCount int               `json:"registered_container_instances_count"`
	RunningTasksCount                 int               `json:"running_tasks_count"`
	PendingTasksCount                 int               `json:"pending_tasks_count"`
	ActiveServicesCount               int               `json:"active_services_count"`
	Region                            string            `json:"region"`
	Tags                              map[string]string `json:"tags"`
}

// awsServerlessRow is the snake_case wire shape for one serverless
// function / service row. Serverless-tier slice 1 chunk 1 (v0.89.90,
// #721 Stream 119). Mirrors scanner.ServerlessInstanceSnapshot — the
// two detection-rule axes (has_trace_axis + has_otel_distro) surface
// as independent booleans so the per-cloud Inventory tab renders
// them as independent badge columns, matching the proposer prompt's
// "either axis presence is informationally surfaced" framing.
//
// Provider + Surface drive the proposer's recommendation-kind prefix
// routing (lambda-* → AWS, cloudrun-* / cloudfunc-* → GCP, azfunc-*
// → Azure, ocifunc-* → OCI). Detail carries surface-specific context
// (Lambda's x_ray_mode + layer_count; Cloud Run's container counts;
// Azure Functions' app_settings subset; OCI Functions' config subset)
// the per-cloud Inventory tab renders as a per-row drilldown.
type awsServerlessRow struct {
	Provider      string     `json:"provider"`
	Surface       string     `json:"surface"`
	AccountID     string     `json:"account_id"`
	Region        string     `json:"region"`
	ResourceName  string     `json:"resource_name"`
	ResourceARN   string     `json:"resource_arn"`
	Runtime       string     `json:"runtime,omitempty"`
	HasTraceAxis  bool       `json:"has_trace_axis"`
	HasOTelDistro bool       `json:"has_otel_distro"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`

	// ColdStartP95Ms / ColdStartExceedsThreshold — Cold-start latency
	// analysis slice 1 chunk 3 (v0.89.115, #753 Stream 151). Mirror
	// the new ServerlessInstanceSnapshot fields with the same nil-
	// elision posture (omitempty pointer) so a Lambda with no
	// observation yet renders the canonical "—" surface in the UI
	// and rows on the non-AWS serverless surfaces (where slice 1
	// has no data) stay nil. The chunk-2 detection result feeds the
	// threshold predicate so the UI doesn't have to re-apply the
	// 1.5x / 500ms rule client-side. See
	// docs/proposals/cold-start-latency-slice1.md §6.2 + §7.
	ColdStartP95Ms            *float64 `json:"cold_start_p95_ms,omitempty"`
	ColdStartExceedsThreshold *bool    `json:"cold_start_exceeds_threshold,omitempty"`

	Detail map[string]any `json:"detail,omitempty"`
}

// awsOrchestrationRow is the snake_case wire shape for one workflow /
// state-machine row. Orchestration-tier slice 1 chunk 1 (v0.89.95,
// #728 Stream 126). Mirrors scanner.OrchestrationInstanceSnapshot —
// the two detection-rule axes (has_trace_axis + has_log_axis) surface
// as independent booleans so the per-cloud Inventory tab renders them
// as independent badge columns, matching the proposer prompt's "either
// axis presence is informationally surfaced" framing.
//
// Provider + Surface drive the proposer's recommendation-kind prefix
// routing (stepfunc-* → AWS, workflows-* → GCP, logicapps-* → Azure).
// Detail carries surface-specific context (Step Functions'
// workflow_type, Workflows' call-log-level, Logic Apps' diagnostic-
// settings subset) the per-cloud Inventory tab renders as a per-row
// drilldown.
type awsOrchestrationRow struct {
	Provider     string         `json:"provider"`
	Surface      string         `json:"surface"`
	AccountID    string         `json:"account_id"`
	Region       string         `json:"region"`
	ResourceName string         `json:"resource_name"`
	ResourceARN  string         `json:"resource_arn"`
	WorkflowType string         `json:"workflow_type,omitempty"`
	HasTraceAxis bool           `json:"has_trace_axis"`
	HasLogAxis   bool           `json:"has_log_axis"`
	LastSeenAt   *time.Time     `json:"last_seen_at,omitempty"`
	Detail       map[string]any `json:"detail,omitempty"`
}

// eventSourceRow is the snake_case wire shape for one inbound event
// source row. Event-source-tier slice 1 chunk 1 (v0.89.100, #734 Stream
// 132). Mirrors scanner.EventSourceInstanceSnapshot — the two detection-
// rule axes (has_trace_axis + has_log_axis) surface as independent
// booleans so the per-cloud Inventory tab renders them as independent
// badge columns, matching the proposer prompt's "either axis presence is
// informationally surfaced" framing.
//
// Provider + Surface drive the proposer's recommendation-kind prefix
// routing (eventbridge-* → AWS, pubsub-* → GCP, servicebus-* → Azure,
// streaming-* → OCI). Detail carries surface-specific context
// (EventBridge's rule_count, Pub/Sub's schema settings subset, Service
// Bus's diagnostic-settings subset, Streaming's logging-target subset)
// the per-cloud Inventory tab renders as a per-row drilldown.
//
// SLICE 1 CHUNK 1 SCHEMAS DEFERRAL: Slice 1 chunk 1 of the
// event-source-tier arc defers the EventBridge Schemas Discoverer axis
// to slice 2 (the Schemas SDK is a separate package and would push chunk
// 1 past its ~1300 LOC budget). The chunk-1 detection rule uses a
// log-target proxy: any rule on the bus with a CloudWatch Logs target
// flips BOTH has_trace_axis AND has_log_axis. Slice 2 separates the two
// axes.
type eventSourceRow struct {
	Provider     string         `json:"provider"`
	Surface      string         `json:"surface"`
	AccountID    string         `json:"account_id"`
	Region       string         `json:"region"`
	ResourceName string         `json:"resource_name"`
	ResourceARN  string         `json:"resource_arn"`
	SourceType   string         `json:"source_type,omitempty"`
	HasTraceAxis bool           `json:"has_trace_axis"`
	HasLogAxis   bool           `json:"has_log_axis"`
	LastSeenAt   *time.Time     `json:"last_seen_at,omitempty"`
	Detail       map[string]any `json:"detail,omitempty"`
	// Event source tier slice 2 (propagation axis). Surfaced to the
	// proposer (the per-message propagation recommendation kinds) and
	// the UI's propagation column. marshalScanResult dropped these
	// before v0.89.194, leaving both consumers dark despite real
	// scanner detection (AWS EventBridge per-rule).
	HasPropagationConfig bool     `json:"has_propagation_config"`
	PropagationNotes     []string `json:"propagation_notes,omitempty"`
}

// marshalScanResult walks the scanner.Result into the snake_case wire
// shape. Empty slices stay empty (never null) so the UI's empty-state
// rendering keys off .length === 0 rather than nil-checking.
// recordTierFailure marks a scan result partial when a handler-dispatched
// tier walk (orchestration, event_source) fails. Without this, a tier
// that errors — most importantly an IAM AccessDenied on the event-source
// scanners — would log a warning but leave Partial=false and an empty
// category slice, so the operator sees "0 event sources" and concludes
// they have none rather than "the scan was denied." It folds into the
// same Partial / PartialReason / FailedServices accumulator the per-
// service walks inside Scan() use. (v0.89.208, found via real-AWS e2e.)
func recordTierFailure(result *scanner.Result, tier string, err error) {
	if result == nil || err == nil {
		return
	}
	result.Partial = true
	result.FailedServices = append(result.FailedServices, tier)
	reason := tier + " discovery failed: " + err.Error()
	if result.PartialReason == "" {
		result.PartialReason = reason
	} else {
		result.PartialReason = result.PartialReason + "; " + reason
	}
}

func marshalScanResult(r *scanner.Result) awsScanResponse {
	out := awsScanResponse{
		ScanID:              r.ScanID,
		ScanStartedAt:       r.ScanStartedAt,
		ScanCompletedAt:     r.ScanCompletedAt,
		AccountID:           r.AccountID,
		Provider:            string(r.Provider),
		Regions:             append([]string{}, r.Regions...),
		Compute:             make([]awsComputeInstanceRow, 0, len(r.Compute)),
		Functions:           make([]awsFunctionRuntimeRow, 0, len(r.Functions)),
		Databases:           make([]awsDatabaseInstanceRow, 0, len(r.Databases)),
		ObjectStores:        make([]awsObjectStoreRow, 0, len(r.ObjectStores)),
		LoadBalancers:       make([]awsLoadBalancerRow, 0, len(r.LoadBalancers)),
		Clusters:            make([]awsClusterRow, 0, len(r.Clusters)),
		DynamoDBTables:      make([]awsDynamoDBTableRow, 0, len(r.DynamoDBTables)),
		ECSClusters:         make([]awsECSClusterRow, 0, len(r.ECSClusters)),
		Serverless:          make([]awsServerlessRow, 0, len(r.Serverless)),
		Orchestrations:      make([]awsOrchestrationRow, 0, len(r.Orchestrations)),
		EventSources:        make([]eventSourceRow, 0, len(r.EventSources)),
		InstrumentedCount:   r.InstrumentedCount,
		UninstrumentedCount: r.UninstrumentedCount,
		Partial:             r.Partial,
		PartialReason:       r.PartialReason,
		FailedServices:      append([]string{}, r.FailedServices...),
	}
	for _, ci := range r.Compute {
		out.Compute = append(out.Compute, awsComputeInstanceRow{
			ResourceID:   ci.ResourceID,
			InstanceType: ci.InstanceType,
			Tags:         ci.Tags,
			HasOTel:      ci.HasOTel,
			OSFamily:     ci.OSFamily,
			Region:       ci.Region,
			LastSeenAt:   ci.LastSeenAt,
		})
	}
	for _, fn := range r.Functions {
		out.Functions = append(out.Functions, awsFunctionRuntimeRow{
			ResourceID:   fn.ResourceID,
			Name:         fn.Name,
			Runtime:      fn.Runtime,
			HasOTelLayer: fn.HasOTelLayer,
			Region:       fn.Region,
		})
	}
	for _, db := range r.Databases {
		// Database tier slice 2 (v0.89.66, #695 Stream 93) — forward
		// the Provider + per-cloud axis flags through the wire shape.
		// AWS rows leave Provider="" so the wire shape stays unchanged
		// for AWS-only callers; GCP / Azure / OCI rows surface their
		// matching axis to the proposer.
		out.Databases = append(out.Databases, awsDatabaseInstanceRow{
			ResourceID:                 db.ResourceID,
			Engine:                     db.Engine,
			EngineVersion:              db.EngineVersion,
			InstanceClass:              db.InstanceClass,
			PerformanceInsightsEnabled: db.PerformanceInsightsEnabled,
			EnhancedMonitoringEnabled:  db.EnhancedMonitoringEnabled,
			Region:                     db.Region,
			Tags:                       db.Tags,
			Provider:                   db.Provider,
			QueryInsightsEnabled:       db.QueryInsightsEnabled,
			SQLInsightsDiagEnabled:     db.SQLInsightsDiagEnabled,
			DatabaseManagementEnabled:  db.DatabaseManagementEnabled,
			LastSeenAt:                 db.LastSeenAt,
		})
	}
	for _, o := range r.ObjectStores {
		out.ObjectStores = append(out.ObjectStores, awsObjectStoreRow{
			ResourceID:                 o.ResourceID,
			Region:                     o.Region,
			ServerAccessLoggingEnabled: o.ServerAccessLoggingEnabled,
			RequestMetricsEnabled:      o.RequestMetricsEnabled,
			Tags:                       o.Tags,
		})
	}
	for _, l := range r.LoadBalancers {
		out.LoadBalancers = append(out.LoadBalancers, awsLoadBalancerRow{
			ResourceID:         l.ResourceID,
			Name:               l.Name,
			Type:               l.Type,
			Scheme:             l.Scheme,
			AccessLogsEnabled:  l.AccessLogsEnabled,
			AccessLogsS3Bucket: l.AccessLogsS3Bucket,
			Region:             l.Region,
			Tags:               l.Tags,
		})
	}
	for _, c := range r.Clusters {
		row := awsClusterRow{
			ResourceID:          c.ResourceID,
			Name:                c.Name,
			KubernetesVersion:   c.KubernetesVersion,
			Status:              c.Status,
			ControlPlaneLogging: append([]string(nil), c.ControlPlaneLogging...),
			Addons:              make([]awsClusterAddonRow, 0, len(c.Addons)),
			NodegroupCount:      c.NodegroupCount,
			FargateProfileCount: c.FargateProfileCount,
			Region:              c.Region,
			Tags:                c.Tags,
			LastSeenAt:          c.LastSeenAt,
		}
		for _, a := range c.Addons {
			row.Addons = append(row.Addons, awsClusterAddonRow{
				Name:    a.Name,
				Version: a.Version,
				Status:  a.Status,
			})
		}
		out.Clusters = append(out.Clusters, row)
	}
	for _, t := range r.DynamoDBTables {
		out.DynamoDBTables = append(out.DynamoDBTables, awsDynamoDBTableRow{
			ResourceID:                t.ResourceID,
			Name:                      t.Name,
			Status:                    t.Status,
			BillingMode:               t.BillingMode,
			ContributorInsightsStatus: t.ContributorInsightsStatus,
			Region:                    t.Region,
			Tags:                      t.Tags,
		})
	}
	// Slice 5 (v0.89.10) — ECS clusters surface alongside the other
	// category arrays. Same non-null posture; the single
	// instrumented-rule axis (container_insights_status) is the
	// string passed verbatim through to the UI.
	for _, c := range r.ECSClusters {
		out.ECSClusters = append(out.ECSClusters, awsECSClusterRow{
			Name:                              c.Name,
			ARN:                               c.ARN,
			Status:                            c.Status,
			ContainerInsightsStatus:           c.ContainerInsightsStatus,
			RegisteredContainerInstancesCount: c.RegisteredContainerInstancesCount,
			RunningTasksCount:                 c.RunningTasksCount,
			PendingTasksCount:                 c.PendingTasksCount,
			ActiveServicesCount:               c.ActiveServicesCount,
			Region:                            c.Region,
			Tags:                              c.Tags,
		})
	}
	// Serverless-tier slice 1 chunk 1 (v0.89.90, #721 Stream 119) —
	// serverless rows surface alongside the other category arrays.
	// Same non-null posture; the two detection-rule axes
	// (has_trace_axis + has_otel_distro) pass verbatim through to
	// the UI alongside the per-surface Detail bag.
	for _, sv := range r.Serverless {
		// Cold-start latency analysis slice 1 chunk 3 (v0.89.115,
		// #753 Stream 151) — forward the two new cold-start fields
		// through the wire shape. The handler-side annotation pass
		// (AnnotateServerlessWithColdStart) populated them in-place
		// against the cold_start_observation latest-row pair before
		// marshalScanResult ran; rows on non-AWS surfaces or with no
		// observation yet carry nil pointers, which the awsServerlessRow
		// omitempty tags elide from the JSON shape.
		out.Serverless = append(out.Serverless, awsServerlessRow{
			Provider:                  sv.Provider,
			Surface:                   sv.Surface,
			AccountID:                 sv.AccountID,
			Region:                    sv.Region,
			ResourceName:              sv.ResourceName,
			ResourceARN:               sv.ResourceARN,
			Runtime:                   sv.Runtime,
			HasTraceAxis:              sv.HasTraceAxis,
			HasOTelDistro:             sv.HasOTelDistro,
			LastSeenAt:                sv.LastSeenAt,
			ColdStartP95Ms:            sv.ColdStartP95Ms,
			ColdStartExceedsThreshold: sv.ColdStartExceedsThreshold,
			Detail:                    sv.Detail,
		})
	}
	// Orchestration-tier slice 1 chunk 1 (v0.89.95, #728 Stream 126) —
	// orchestration rows surface alongside the other category arrays.
	// Same non-null posture; the two detection-rule axes
	// (has_trace_axis + has_log_axis) pass verbatim through to the UI
	// alongside the per-surface Detail bag. AWS Step Functions is the
	// only populated surface in chunk 1; chunks 2 + 3 add GCP
	// Workflows and Azure Logic Apps. The loop iterates over an empty
	// slice when the orchestration tier is not in the request's Tiers
	// list (runAWSScan only invokes ScanOrchestrations on that tier).
	for _, oc := range r.Orchestrations {
		out.Orchestrations = append(out.Orchestrations, awsOrchestrationRow{
			Provider:     oc.Provider,
			Surface:      oc.Surface,
			AccountID:    oc.AccountID,
			Region:       oc.Region,
			ResourceName: oc.ResourceName,
			ResourceARN:  oc.ResourceARN,
			WorkflowType: oc.WorkflowType,
			HasTraceAxis: oc.HasTraceAxis,
			HasLogAxis:   oc.HasLogAxis,
			LastSeenAt:   oc.LastSeenAt,
			Detail:       oc.Detail,
		})
	}
	// Event-source-tier slice 1 chunk 1 (v0.89.100, #734 Stream 132) —
	// event source rows surface alongside the other category arrays.
	// Same non-null posture; the two detection-rule axes (has_trace_axis
	// + has_log_axis) pass verbatim through to the UI alongside the
	// per-surface Detail bag. AWS EventBridge is the only populated
	// surface in chunk 1; chunks 2 + 3 + 4 add GCP Pub/Sub, Azure
	// Service Bus, and OCI Streaming.
	out.EventSources = marshalEventSourceRows(r.EventSources)
	return out
}

// marshalEventSourceRows converts scanner event-source snapshots into
// the snake_case wire rows shared by every cloud's scan response. The
// AWS response (marshalScanResult) and the GCP / Azure / OCI scan
// handlers all call this so the event-source wire shape — including the
// slice-2 propagation axis (v0.89.194) — stays identical across clouds.
// Returns a non-nil empty slice for empty input so the wire renders []
// rather than null (the per-cloud Inventory tab's contract).
func marshalEventSourceRows(snaps []scanner.EventSourceInstanceSnapshot) []eventSourceRow {
	rows := make([]eventSourceRow, 0, len(snaps))
	for _, es := range snaps {
		rows = append(rows, eventSourceRow{
			Provider:             es.Provider,
			Surface:              es.Surface,
			AccountID:            es.AccountID,
			Region:               es.Region,
			ResourceName:         es.ResourceName,
			ResourceARN:          es.ResourceARN,
			SourceType:           es.SourceType,
			HasTraceAxis:         es.HasTraceAxis,
			HasLogAxis:           es.HasLogAxis,
			LastSeenAt:           es.LastSeenAt,
			Detail:               es.Detail,
			HasPropagationConfig: es.HasPropagationConfig,
			PropagationNotes:     es.PropagationNotes,
		})
	}
	return rows
}

// HandleAWSRunScan — POST /api/v1/discovery/aws/connections/:id/scan.
//
// Looks up the stored connection by account_id, emits a scan_started
// audit event, runs the scanner synchronously, emits a scan_completed
// event with per-category counts and the partial flag, then returns
// the typed scanner.Result as JSON.
//
// Known trade-off: the scan blocks the HTTP request. A large account
// (50k+ resources) could hang for minutes. Acceptable for slice 1 per
// the design doc's "Failure modes" section; slice 3 introduces
// scheduled scans with the result persisted asynchronously, at which
// point this endpoint can shrink to "scan_id, queued" semantics. The
// route stays stable.
//
// Audit invariants per the design doc's "Audit trail invariants"
// section:
//   - discovery.aws.scan_started fires BEFORE the scan begins
//   - discovery.aws.scan_completed fires AFTER the scan returns, with
//     compute_count, function_count, database_count,
//     object_store_count (slice 3a / v0.88.0), load_balancer_count
//     (slice 3a / v0.88.0), cluster_count (slice 3b / v0.89.0),
//     dynamodb_count + instrumented_dynamodb_count (slice 4 /
//     v0.89.6), ecs_count + instrumented_ecs_count (slice 5 /
//     v0.89.10), instrumented_count, uninstrumented_count, the
//     partial flag, partial_reason (the operator-visible explanation
//     when partial is true), and failed_services (structured list
//     of service identifiers —
//     "ec2"/"lambda"/"rds"/"s3"/"alb"/"eks"/"dynamodb"/"ecs"/"assume_role"
//     — for SIEM forwarders and the proposer's future scan-history
//     loop to pattern-match against) in the payload
//   - both events carry the account_id and (for scan_completed) the
//     scan_id, so an auditor can reconstruct any scan's lifecycle from
//     the audit log alone
func (h *DiscoveryHandlers) HandleAWSRunScan(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingAccountID",
			Message: "Account ID path parameter is required.",
		}})
		return
	}

	// Body is optional — the empty-body path falls back to the
	// connection's stored Regions. Parse failures fall through to the
	// empty-body branch rather than 400ing; the operator's intent is
	// "scan whatever's configured" and we honor that even when the
	// browser sent no payload.
	var req awsRunScanRequest
	_ = c.ShouldBindJSON(&req)

	// Serverless-tier slice 1 chunk 1 (v0.89.90, #721 Stream 119) —
	// normalize the optional Tiers field. parseTiersOrDefault drops
	// unknown values and falls back to DefaultScanTiers (which now
	// includes "serverless") on empty input. The normalized list is
	// validated even though the AWS scanner currently walks every
	// tier — chunk 5 wires the filter through once the per-tier UI
	// lands, and the validated parse path means the wire shape stays
	// stable.
	req.Tiers = parseTiersOrDefault(req.Tiers)

	// runAWSScan emits scan_started + scan_completed audit events and
	// drives the scanner. The single-account endpoint passes an empty
	// scanAllID so the per-account scan_completed event omits the
	// scan_all_id field (v0.89.7a trace linkage is only present when
	// the orchestrator drives the per-account scan). The third return
	// value is the HTTP status the handler should emit on failure;
	// the orchestrator path ignores it.
	result, herr, status := h.runAWSScan(c.Request.Context(), accountID, req.Regions, req.Tiers, "" /* scanAllID */)
	if herr != nil {
		c.JSON(status, gin.H{"error": herr})
		return
	}
	c.JSON(http.StatusOK, marshalScanResult(result))
}

// runAWSScan is the reusable per-account scan body — extracted from
// HandleAWSRunScan in v0.89.7a (#616 Stream 21) so the multi-account
// orchestrator can drive the same path without re-implementing the
// audit emission or the partial-failure plumbing. The single-account
// HTTP handler keeps its existing observable behavior: same audit
// shape, same error envelope, same response.
//
// scanAllID is the v0.89.7a trace link tying a per-account scan to
// the aggregate scan_all_completed event. The orchestrator passes
// the UUID it generated at the top of the fan-out; the single-
// account endpoint passes "". Empty values are omitted from the
// per-account scan_completed payload (mirrors the conditional-insert
// idiom for partial_reason / failed_services — map[string]any does
// not honor omitempty).
//
// Return shape:
//   - (*scanner.Result, nil, 0) on success
//   - (nil, *HumanizedError, http.StatusXxx) on failure
//
// The scan_started event always fires before any failure that can
// happen after the credstore lookup — the design doc's
// "scan_started without scan_completed implies failure" invariant
// is preserved here. ConnectionNotFound (404) and the pre-flight
// wiring 500s happen BEFORE scan_started, matching the slice-1
// posture (TestHandleAWSRunScan_NotFound asserts zero audit entries
// for a 404).
func (h *DiscoveryHandlers) runAWSScan(ctx context.Context, accountID string, requestedRegions []string, requestedTiers []string, scanAllID string) (*scanner.Result, *scanner.HumanizedError, int) {
	if h.credStore == nil {
		return nil, &scanner.HumanizedError{
			Code:          "CredStoreNotWired",
			Message:       "Squadron's credential substrate isn't configured.",
			SuggestedStep: "save",
		}, http.StatusInternalServerError
	}
	if h.awsScannerFor == nil {
		return nil, &scanner.HumanizedError{
			Code:          "ScannerNotWired",
			Message:       "Squadron's scanner factory isn't configured. The Save flow wires it via the credstore key.",
			SuggestedStep: "save",
		}, http.StatusInternalServerError
	}

	conn, err := h.credStore.GetConnection(ctx, accountID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws run scan: credstore read failed", zap.Error(err), zap.String("account_id", accountID))
		}
		return nil, &scanner.HumanizedError{
			Code:          "CredStoreReadFailed",
			Message:       "Squadron could not read the connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}, http.StatusInternalServerError
	}
	if conn == nil {
		return nil, &scanner.HumanizedError{
			Code:    "ConnectionNotFound",
			Message: "No AWS connection exists with that account ID. Connect the account from the wizard first.",
		}, http.StatusNotFound
	}

	// Resolve the regions to scan. Empty request body falls back to
	// the connection's stored list — slice 1 ships single-entry lists,
	// slice 3 will iterate.
	regions := requestedRegions
	if len(regions) == 0 {
		regions = append([]string(nil), conn.Regions...)
	}

	// scan_started fires BEFORE the scan call. If the scanner factory
	// fails to build a scanner (e.g. credential decryption error), the
	// audit trail still shows the operator's intent — a forensic reader
	// can see a scan_started with no matching scan_completed and infer
	// the failure happened before the scan began. Mirrors the
	// "Squadron crashes mid-scan" failure-mode contract.
	if h.auditService != nil {
		startedPayload := map[string]any{
			"account_id":  accountID,
			"regions":     regions,
			"recorded_at": time.Now().UTC(),
		}
		// v0.89.7a (#616 Stream 21) — when the orchestrator drives
		// the per-account scan, scan_started also carries the
		// scan_all_id so a forensic reader can correlate a stranded
		// scan_started (no matching scan_completed) with the
		// scan_all_completed aggregate event. Single-account calls
		// pass "" and the field stays out of the payload.
		if scanAllID != "" {
			startedPayload["scan_all_id"] = scanAllID
		}
		_ = h.auditService.Record(ctx, services.AuditEntry{
			Actor:      "system",
			EventType:  "discovery.aws.scan_started",
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   accountID,
			Action:     "scan_started",
			Payload:    startedPayload,
		})
	}

	awsScanner, err := h.awsScannerFor(conn)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws run scan: scanner construction failed", zap.Error(err), zap.String("account_id", accountID))
		}
		return nil, &scanner.HumanizedError{
			Code:          "ScannerConstructFailed",
			Message:       "Squadron could not decrypt the connection credentials. Re-validate from the wizard.",
			SuggestedStep: "validate",
		}, http.StatusInternalServerError
	}

	// Same defense-in-depth posture as HandleAWSValidate: even though
	// the underlying SDK calls are bounded, the HTTP handler enforces
	// its own ceiling. 5 minutes accommodates large-account scans
	// (the design doc's slice-1 upper bound) while still preventing
	// an indefinite hang if a future regression introduces one.
	scanCtx, scanCancel := context.WithTimeout(ctx, scanHandlerTimeout)
	defer scanCancel()
	// Orchestration-tier slice 1 chunk 1 (v0.89.95, #728 Stream 126):
	// resolve the tier filter once. Empty / unrecognized fall through
	// to DefaultScanTiers (which now includes "orchestration"), so the
	// default path scans every supported tier. parseTiersOrDefault is
	// the same helper the request handler uses so the per-request
	// validation contract is preserved through the scan-all dispatch
	// path too.
	tiers := parseTiersOrDefault(requestedTiers)
	result, err := awsScanner.Scan(scanCtx, conn, regions)
	if err != nil {
		// Per scanner.Scanner's contract the AWS implementation sets
		// Partial=true with PartialReason rather than returning a Go
		// error. A non-nil error here means a future implementation
		// broke that contract — surface as 500 with the humanized
		// error.
		if h.logger != nil {
			h.logger.Error("aws run scan: scanner returned error", zap.Error(err), zap.String("account_id", accountID))
		}
		// HumanizeError lives in the aws package — and importing it
		// here would pull the SDK into the handler's import graph. The
		// scanner's Result.PartialReason already carries the humanized
		// text on the contract-honoring path; the fallback uses the
		// raw error string.
		return nil, &scanner.HumanizedError{
			Code:          "ScannerInternal",
			Message:       "Scan failed unexpectedly: " + err.Error(),
			SuggestedStep: "validate",
		}, http.StatusInternalServerError
	}

	// Orchestration-tier slice 1 chunk 1 (v0.89.95, #728 Stream 126).
	// When the request's normalized tier list contains "orchestration"
	// and the underlying scanner satisfies the optional
	// OrchestrationDiscoveryScanner interface, dispatch a per-region
	// orchestration walk and fold the snapshots into result.Orchestrations.
	// Scanners that don't satisfy the interface (chunks 2/3's GCP +
	// Azure scanners ship that capability, OCI does not) are silently
	// no-op'd — the per-cloud Inventory tab renders an empty
	// orchestrations slice. Storage persistence lives in chunk 4 of
	// the arc; this chunk only surfaces orchestration rows on the
	// HTTP wire shape.
	if result != nil && tierListContains(tiers, TierOrchestration) {
		if orchScanner, ok := awsScanner.(OrchestrationDiscoveryScanner); ok {
			orchOut, orchErr := orchScanner.ScanOrchestrations(scanCtx, scanner.ScanScope{
				AccountID: accountID,
				Regions:   regions,
			})
			if orchErr != nil {
				// Match the per-service partial-failure posture used
				// throughout Scan(): an orchestration walk failure
				// degrades the response (empty orchestration slice)
				// rather than 500-ing the whole scan. The logger
				// captures the diagnostic; the operator-visible
				// signal is the absence of orchestration rows.
				if h.logger != nil {
					h.logger.Warn("aws run scan: orchestration scan failed",
						zap.Error(orchErr), zap.String("account_id", accountID))
				}
				recordTierFailure(result, "orchestration", orchErr)
			}
			if len(orchOut) > 0 {
				result.Orchestrations = append(result.Orchestrations, orchOut...)
			}
		}
	}

	// Event-source-tier slice 1 chunk 1 (v0.89.100, #734 Stream 132).
	// When the request's normalized tier list contains "event_source" and
	// the underlying scanner satisfies the optional
	// EventSourceDiscoveryScanner interface, dispatch a per-region event
	// source walk and fold the snapshots into result.EventSources.
	// Scanners that don't satisfy the interface (chunks 2/3/4's GCP +
	// Azure + OCI scanners ship their per-cloud surfaces) are silently
	// no-op'd — the per-cloud Inventory tab renders an empty event_sources
	// slice. Storage persistence and the per-provider rollup endpoints
	// live in chunk 5 of the arc; this chunk only surfaces event source
	// rows on the HTTP wire shape.
	if result != nil && tierListContains(tiers, TierEventSource) {
		if esScanner, ok := awsScanner.(EventSourceDiscoveryScanner); ok {
			esOut, esErr := esScanner.ScanEventSources(scanCtx, scanner.ScanScope{
				AccountID: accountID,
				Regions:   regions,
			})
			if esErr != nil {
				// Match the per-service partial-failure posture used
				// throughout Scan(): an event source walk failure
				// degrades the response (empty event_sources slice)
				// rather than 500-ing the whole scan. The logger
				// captures the diagnostic; the operator-visible signal
				// is the absence of event source rows.
				if h.logger != nil {
					h.logger.Warn("aws run scan: event source scan failed",
						zap.Error(esErr), zap.String("account_id", accountID))
				}
				recordTierFailure(result, "event_source", esErr)
			}
			if len(esOut) > 0 {
				result.EventSources = append(result.EventSources, esOut...)
			}
		}
	}

	if h.auditService != nil {
		// v0.87.4: scan_completed payload uses conditional inserts for
		// partial_reason and failed_services so the audit shape mirrors
		// the HTTP response's omitempty semantics. map[string]any does
		// NOT honor JSON-tag omitempty (only struct fields do), so an
		// unconditional insert of an empty string or nil slice would
		// emit "partial_reason": "" and "failed_services": null on
		// every successful scan — line noise that audit consumers
		// (SIEM forwarders, Timeline UI, squadronctl, proposer's
		// future learning loop) would have to filter out per event.
		// The happy path now emits ONLY the mandatory fields; the
		// failure path emits the same plus partial_reason +
		// failed_services. Symmetric with the typed HTTP response.
		payload := map[string]any{
			"account_id":     accountID,
			"scan_id":        result.ScanID,
			"compute_count":  len(result.Compute),
			"function_count": len(result.Functions),
			"database_count": len(result.Databases),
			// Slice 3a (v0.88.0) — object_store_count +
			// load_balancer_count join the audit payload as
			// MANDATORY fields (always present, never omitempty).
			// Same posture as compute_count / function_count /
			// database_count: an operator skimming the audit
			// timeline should see the slice 3a categories' counts
			// even on the happy path. Empty inventories emit "0";
			// they do NOT drop out via omitempty.
			"object_store_count":  len(result.ObjectStores),
			"load_balancer_count": len(result.LoadBalancers),
			// Slice 3b (v0.89.0) — cluster_count joins the audit
			// payload as a MANDATORY field (always present, never
			// omitempty). Same posture as compute_count /
			// function_count / database_count / object_store_count /
			// load_balancer_count: an operator skimming the audit
			// timeline should see the slice 3b cluster category's
			// count even on the happy path. Empty inventories emit
			// "0"; they do NOT drop out via omitempty.
			"cluster_count": len(result.Clusters),
			// Slice 4 (v0.89.6) — dynamodb_count joins the audit
			// payload as a MANDATORY field (always present, never
			// omitempty). instrumented_dynamodb_count surfaces the
			// per-category instrumented tally alongside it so an
			// operator skimming the audit timeline can see DynamoDB
			// coverage without recomputing from the overall
			// instrumented_count. The two fields move together —
			// dropping either into the conditional-insert path
			// would lose the operator-visible coverage signal on
			// happy-path scans.
			"dynamodb_count":              len(result.DynamoDBTables),
			"instrumented_dynamodb_count": countInstrumentedDynamoDB(result.DynamoDBTables),
			// Slice 5 (v0.89.10) — ecs_count joins the audit payload
			// as a MANDATORY field (always present, never omitempty).
			// instrumented_ecs_count surfaces the per-category
			// instrumented tally alongside it so an operator skimming
			// the audit timeline can see ECS coverage without
			// recomputing from the overall instrumented_count. The
			// two fields move together — dropping either into the
			// conditional-insert path would lose the operator-visible
			// coverage signal on happy-path scans. Same posture as
			// the slice 4 DynamoDB pair.
			"ecs_count":              len(result.ECSClusters),
			"instrumented_ecs_count": countInstrumentedECS(result.ECSClusters),
			"instrumented_count":     result.InstrumentedCount,
			"uninstrumented_count":   result.UninstrumentedCount,
			"partial":                result.Partial,
			"recorded_at":            time.Now().UTC(),
		}
		if result.PartialReason != "" {
			payload["partial_reason"] = result.PartialReason
		}
		if len(result.FailedServices) > 0 {
			payload["failed_services"] = result.FailedServices
		}
		// v0.89.7a (#616 Stream 21) — scan_all_id is the trace link
		// tying this per-account event to the aggregate
		// scan_all_completed event. Only present when the
		// orchestrator drove the scan; single-account calls (via
		// the unchanged /discovery/aws/connections/:id/scan
		// endpoint) pass "" and the field drops out, preserving
		// the existing per-account event shape verbatim.
		if scanAllID != "" {
			payload["scan_all_id"] = scanAllID
		}
		_ = h.auditService.Record(ctx, services.AuditEntry{
			Actor:      "system",
			EventType:  "discovery.aws.scan_completed",
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   accountID,
			Action:     "scan_completed",
			Payload:    payload,
		})
	}

	// Trace integration slice 1 chunk 4 (v0.89.77) — annotate the
	// per-resource last_seen_at in-place against the traceindex
	// before the response is serialized. Nil traceIndex short-
	// circuits the call entirely; a flaky index logs warnings but
	// never breaks the scan endpoint. The annotation MUST run after
	// scan_completed audit emits so the audit payload's counts are
	// unaffected by the join (slice 1 contract item 7).
	if h.traceIndex != nil && result != nil {
		AnnotateComputeWithLastSeen(ctx, h.traceIndex, "aws", accountID, result.Compute, h.logger)
		AnnotateDatabaseWithLastSeen(ctx, h.traceIndex, "aws", accountID, result.Databases, h.logger)
		AnnotateClusterWithLastSeen(ctx, h.traceIndex, "aws", accountID, result.Clusters, h.logger)
	}

	// Cold-start latency analysis slice 1 chunk 3 (v0.89.115, #753
	// Stream 151) — annotate the per-Lambda cold_start_p95_ms +
	// cold_start_exceeds_threshold fields in-place against the
	// persisted cold_start_observation table. Both nil store + nil
	// thresholds short-circuit the call. The pass runs AFTER the
	// trace-emission annotations so the audit emit ordering is
	// unaffected and the per-row JSON shape gathers both annotation
	// passes' output before marshalScanResult serializes.
	if h.coldStartStore != nil && h.coldStartConstants != nil && result != nil {
		AnnotateServerlessWithColdStart(ctx, h.coldStartStore, h.coldStartConstants, result.Serverless, h.logger)
	}

	// Error rate correlation slice 1 chunk 3 (v0.89.129, #769
	// Stream 167) — annotate the per-Serverless current_error_rate
	// + error_rate_exceeds_threshold fields in-place against the
	// persisted error_rate_observation table. Runs AFTER the
	// cold-start annotation so the per-row JSON shape gathers all
	// three annotation passes' output before marshalScanResult
	// serializes. Nil store short-circuits.
	if h.errorRateStore != nil && result != nil {
		AnnotateServerlessWithErrorRate(ctx, h.errorRateStore, result.Serverless, h.logger)
	}

	return result, nil, 0
}

// --- HandleAWSScanAll (v0.89.7a Stream 21, #616) --------------------

// awsScanAllResponse is the snake_case wire shape returned by the
// multi-account scan-all endpoint. Mirrors awsorch.ScanAllResult
// with json tags so the client receives a stable schema.
//
// The aggregate fields (total_resources, total_instrumented,
// total_uninstrumented) sum across the per-account categories the
// per-account scans produce — that's the v0.89.7a aggregate roll-up
// posture. The aggregate does NOT enumerate per-service counts; the
// per-account events still carry compute_count/function_count/etc.
// for operators who want per-service drill-down.
//
// The wire shape is intentionally append-only — failed_accounts is
// a structured list (account_id + error_code + humanized_message)
// so a SIEM forwarder or the ops-script that called the endpoint
// can pattern-match on the codes without parsing prose. Snake_case
// throughout, matching the per-account endpoint's posture.
type awsScanAllResponse struct {
	ScanAllID           string                 `json:"scan_all_id"`
	TotalAccounts       int                    `json:"total_accounts"`
	SucceededAccounts   []awsScanAllAccountRow `json:"succeeded_accounts"`
	FailedAccounts      []awsScanAllFailureRow `json:"failed_accounts"`
	TotalResources      int                    `json:"total_resources"`
	TotalInstrumented   int                    `json:"total_instrumented"`
	TotalUninstrumented int                    `json:"total_uninstrumented"`
	Partial             bool                   `json:"partial"`
	// Concurrency surfaces the effective bound the orchestrator
	// used after defaults + cap were applied. Operators who asked
	// for 10 see 8 here; operators who omitted the parameter see
	// the default. Useful diagnostic surface for ops scripts that
	// log responses.
	Concurrency int `json:"concurrency"`
}

// awsScanAllAccountRow mirrors awsorch.AccountScanResult on the
// wire. Per-account counts are surfaced so a caller can compute
// per-account coverage ratios without round-tripping the per-
// account scan endpoint.
type awsScanAllAccountRow struct {
	AccountID           string `json:"account_id"`
	ScanID              string `json:"scan_id"`
	ResourceCount       int    `json:"resource_count"`
	InstrumentedCount   int    `json:"instrumented_count"`
	UninstrumentedCount int    `json:"uninstrumented_count"`
}

// awsScanAllFailureRow mirrors awsorch.AccountScanFailure on the
// wire. error_code is the stable identifier (matches the per-
// account handler's HumanizedError.Code convention);
// humanized_message is the operator-visible prose. Neither field
// ever carries credential material — the orchestrator never sees
// cleartext credentials.
type awsScanAllFailureRow struct {
	AccountID        string `json:"account_id"`
	ErrorCode        string `json:"error_code"`
	HumanizedMessage string `json:"humanized_message"`
}

// HandleAWSScanAll — POST /api/v1/discovery/aws/scan-all.
//
// v0.89.7a Stream 21 (#616) — the multi-account scan-all endpoint.
// Fans out per-account scans across every stored AWS connection in
// the credstore, with bounded concurrency, aggregates the result,
// and emits one discovery.aws.scan_all_completed audit event.
//
// Optional query parameters:
//   - regions (comma-separated): per-call region override applied
//     to every connection. Empty falls back to each connection's
//     stored region list — same posture as the per-account
//     endpoint's empty-body branch.
//   - concurrency (int): max simultaneous per-account scans.
//     Defaults to awsorch.DefaultScanAllConcurrency (3) when
//     unset or <= 0; capped at awsorch.MaxScanAllConcurrency (8)
//     when above. The effective value is surfaced back in the
//     response's concurrency field.
//
// Audit invariants:
//   - Per-account scan_started + scan_completed events still fire
//     unchanged (the orchestrator drives the existing runAWSScan
//     method). Both events additionally carry the scan_all_id so
//     a forensic reader can correlate them with the aggregate
//     event.
//   - One discovery.aws.scan_all_completed event fires after the
//     fan-out completes — with the aggregate counts, the failed
//     accounts list, and the partial flag. Operators reading the
//     timeline see: N per-account events, then one aggregate
//     event with the same scan_all_id linking them.
//   - Zero connections still emits the aggregate event so the
//     operator's intent is visible in the timeline (proof the
//     operation ran, even with nothing to do).
//
// Partial-failure posture: one account's failure does not block
// the rest. The failed account lands in failed_accounts with its
// error_code + humanized_message; the rest of the fan-out
// continues unaffected. The aggregate's partial field is true
// when any account failed.
func (h *DiscoveryHandlers) HandleAWSScanAll(c *gin.Context) {
	if h.credStore == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreNotWired",
			Message:       "Squadron's credential substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.awsScannerFor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "ScannerNotWired",
			Message:       "Squadron's scanner factory isn't configured. The Save flow wires it via the credstore key.",
			SuggestedStep: "save",
		}})
		return
	}

	regions := parseScanAllRegions(c.Query("regions"))
	concurrency := parseScanAllConcurrency(c.Query("concurrency"))

	// Build the orchestrator per-request. The dependencies are the
	// credstore (for ListConnections) and a closure that adapts
	// h.runAWSScan to the PerAccountScan signature. The adapter
	// converts the handler's (result, *HumanizedError, status) into
	// the orchestrator's (result, *AccountScanFailure) shape; the
	// HTTP status is dropped (the orchestrator aggregates failures
	// rather than rendering one).
	orch := awsorch.NewOrchestrator(h.credStore, func(ctx context.Context, conn *credstore.CloudConnection, regs []string, scanAllID string) (*scanner.Result, *awsorch.AccountScanFailure) {
		// Orchestration-tier slice 1 chunk 1 (v0.89.95, #728 Stream
		// 126): the scan-all path uses the default tier surface — it
		// does not currently surface a per-account tier filter.
		// Passing nil triggers parseTiersOrDefault inside runAWSScan
		// (defense-in-depth) and DefaultScanTiers includes
		// orchestration so every per-account scan emits orchestration
		// rows.
		result, herr, _ := h.runAWSScan(ctx, conn.AccountID, regs, nil /* tiers */, scanAllID)
		if herr != nil {
			msg := herr.Message
			if msg == "" {
				msg = "Per-account scan failed."
			}
			return nil, &awsorch.AccountScanFailure{
				AccountID:        conn.AccountID,
				ErrorCode:        herr.Code,
				HumanizedMessage: msg,
			}
		}
		return result, nil
	})

	scanAllResult, err := orch.ScanAll(c.Request.Context(), awsorch.ScanAllRequest{
		Regions:     regions,
		Concurrency: concurrency,
	})
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws scan-all: orchestrator failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "ScanAllInternal",
			Message:       "Squadron could not start the multi-account scan. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	effective := awsorch.NormalizeConcurrency(concurrency)

	if h.auditService != nil {
		// Aggregate audit event. Per the v0.89.7a spec:
		//   - total/succeeded/failed account lists
		//   - total_resources / total_instrumented / total_uninstrumented
		//     (rolled up across all per-service categories)
		//   - partial flag (true when any failed_accounts present)
		// Never includes credential material — the orchestrator never
		// sees cleartext credentials.
		failedRows := make([]map[string]any, 0, len(scanAllResult.Failed))
		failedAccountIDs := make([]string, 0, len(scanAllResult.Failed))
		for _, f := range scanAllResult.Failed {
			failedRows = append(failedRows, map[string]any{
				"account_id":        f.AccountID,
				"error_code":        f.ErrorCode,
				"humanized_message": f.HumanizedMessage,
			})
			failedAccountIDs = append(failedAccountIDs, f.AccountID)
		}
		payload := map[string]any{
			"scan_all_id":          scanAllResult.ScanAllID,
			"total_accounts":       scanAllResult.TotalAccounts,
			"succeeded_accounts":   len(scanAllResult.Succeeded),
			"failed_accounts":      failedRows,
			"total_resources":      scanAllResult.TotalResources,
			"total_instrumented":   scanAllResult.TotalInstrumented,
			"total_uninstrumented": scanAllResult.TotalUninstrumented,
			"partial":              scanAllResult.Partial,
			"recorded_at":          time.Now().UTC(),
		}
		if len(failedAccountIDs) > 0 {
			// Convenience top-level list of failed account IDs for
			// SIEM forwarders that pattern-match on flat fields
			// rather than nested structs. The full structured
			// failed_accounts above carries the same IDs plus the
			// codes + prose.
			payload["failed_account_ids"] = failedAccountIDs
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryAWSScanAllCompleted,
			TargetType: services.AuditTargetDiscoveryScanAll,
			TargetID:   scanAllResult.ScanAllID,
			Action:     "scan_all_completed",
			Payload:    payload,
		})
	}

	c.JSON(http.StatusOK, marshalScanAllResult(scanAllResult, effective))
}

// parseScanAllRegions splits the optional comma-separated regions
// query parameter into a slice. Empty / missing returns nil so the
// orchestrator's empty-Regions fallback kicks in (each connection
// uses its stored region list).
func parseScanAllRegions(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseScanAllConcurrency parses the optional concurrency query
// parameter. Empty / non-numeric returns 0 so the orchestrator's
// normalizeConcurrency falls back to DefaultScanAllConcurrency.
// The cap (MaxScanAllConcurrency) is enforced inside the
// orchestrator — this function does not pre-clamp; passing the
// raw value through means the response's concurrency field still
// shows the effective post-cap value.
func parseScanAllConcurrency(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// marshalScanAllResult walks the orchestrator's aggregate into the
// snake_case wire shape. Empty slices stay empty (never null) so
// the UI's empty-state branch is a single .length === 0 check —
// matching the per-account endpoint's posture.
func marshalScanAllResult(r *awsorch.ScanAllResult, effectiveConcurrency int) awsScanAllResponse {
	out := awsScanAllResponse{
		ScanAllID:           r.ScanAllID,
		TotalAccounts:       r.TotalAccounts,
		SucceededAccounts:   make([]awsScanAllAccountRow, 0, len(r.Succeeded)),
		FailedAccounts:      make([]awsScanAllFailureRow, 0, len(r.Failed)),
		TotalResources:      r.TotalResources,
		TotalInstrumented:   r.TotalInstrumented,
		TotalUninstrumented: r.TotalUninstrumented,
		Partial:             r.Partial,
		Concurrency:         effectiveConcurrency,
	}
	for _, s := range r.Succeeded {
		out.SucceededAccounts = append(out.SucceededAccounts, awsScanAllAccountRow{
			AccountID:           s.AccountID,
			ScanID:              s.ScanID,
			ResourceCount:       s.ResourceCount,
			InstrumentedCount:   s.InstrumentedCount,
			UninstrumentedCount: s.UninstrumentedCount,
		})
	}
	for _, f := range r.Failed {
		out.FailedAccounts = append(out.FailedAccounts, awsScanAllFailureRow{
			AccountID:        f.AccountID,
			ErrorCode:        f.ErrorCode,
			HumanizedMessage: f.HumanizedMessage,
		})
	}
	return out
}

// marshalValidationResult flattens scanner.ValidationResult into the
// wire shape. Walks the preflight rows once to surface a top-level
// errors[] the UI can render without re-walking the typed struct.
func marshalValidationResult(vr *scanner.ValidationResult) awsValidateResponse {
	out := awsValidateResponse{
		AssumeRoleOK:  vr.AssumeRoleOK,
		AssumeRoleErr: vr.AssumeRoleErr,
		Preflight:     make([]awsValidatePreflightRow, 0, len(vr.Preflight)),
	}
	if vr.AssumeRoleErr != nil {
		out.Errors = append(out.Errors, *vr.AssumeRoleErr)
	}
	for _, p := range vr.Preflight {
		row := awsValidatePreflightRow{
			Service:     p.Service,
			OK:          p.OK,
			SampleCount: p.SampleCount,
			Err:         p.Err,
		}
		out.Preflight = append(out.Preflight, row)
		if p.Err != nil {
			out.Errors = append(out.Errors, *p.Err)
		}
	}
	return out
}

// --- HandleAWSGenerateRecommendations (Stream 2F) -------------------

// awsGenerateRecommendationsRequest is the JSON wire shape the
// Inventory tab POSTs after a scan completes and the operator clicks
// "Generate recommendations". The scan_result is the typed
// scanner-result body the same operator just rendered; the handler
// converts it into an ai.DiscoveryScanContext, calls the proposer,
// and walks the plan-kind response into discovery-source
// Recommendations.
type awsGenerateRecommendationsRequest struct {
	ScanResult awsScanResponse `json:"scan_result"`
}

// awsGenerateRecommendationsResponse is the wire shape the
// Recommendations tab consumes. When the proposer declines (no
// productive plan), Declined is true and Reason carries the model's
// explanation; the Recommendations array is empty. Otherwise
// Recommendations is one entry per plan step the model emitted.
type awsGenerateRecommendationsResponse struct {
	Declined        bool                             `json:"declined"`
	Reason          string                           `json:"reason,omitempty"`
	Reasoning       string                           `json:"reasoning,omitempty"`
	Recommendations []recommendations.Recommendation `json:"recommendations"`
}

// HandleAWSGenerateRecommendations — POST
// /api/v1/discovery/aws/connections/:id/recommendations.
//
// Flow:
//  1. Validate request body shape. 400 on malformed JSON.
//  2. Verify the account_id in the URL matches scan_result.account_id —
//     a mismatch is operator error (e.g. accidentally scrolled to the
//     wrong account's inventory) and should fail loudly before the
//     proposer runs.
//  3. Look up the connection via credstore.GetConnection. 404 on
//     "no row matches"; the recommendations route exists only for
//     accounts Squadron is configured to scan.
//  4. Convert scanner.Result → ai.DiscoveryScanContext, then call
//     aiProposer.ProposeFromDiscoveryScan.
//  5. If declined: 200 with declined=true + the reason. No
//     recommendations_generated audit event (nothing was generated).
//  6. Otherwise: walk the plan-kind result into recommendation rows —
//     one per plan step — and emit the
//     discovery.aws.recommendations_generated audit event with
//     {scan_id, account_id, step_count, tokens_in, tokens_out}. The
//     audit payload does NOT include the Terraform content; audit logs
//     shouldn't grow with snippet size.
//
// Returns 200 with the typed response on success. 400/404/500 per the
// flow above.
// mapEventSourceCandidates converts the shared event-source wire rows
// into proposer EventSourceCandidates. Used by every cloud's
// generate-recommendations handler — the wire row and candidate shapes
// are provider-agnostic. Reads the DLQ-axis Detail keys defensively
// (Detail is map[string]any; numbers arrive as float64 after a JSON
// round-trip, so handle int / int64 / float64).
func mapEventSourceCandidates(rows []eventSourceRow) []ai.EventSourceCandidate {
	out := make([]ai.EventSourceCandidate, 0, len(rows))
	for _, es := range rows {
		cand := ai.EventSourceCandidate{
			Provider:             es.Provider,
			Surface:              es.Surface,
			SourceType:           es.SourceType,
			ResourceName:         es.ResourceName,
			ResourceARN:          es.ResourceARN,
			Region:               es.Region,
			HasTraceAxis:         es.HasTraceAxis,
			HasLogAxis:           es.HasLogAxis,
			HasPropagationConfig: es.HasPropagationConfig,
			PropagationNotes:     es.PropagationNotes,
		}
		if es.Detail != nil {
			if v, ok := es.Detail["has_dlq"].(bool); ok {
				cand.HasDLQ = v
			}
			if v, ok := es.Detail["redrive_policy_target_arn"].(string); ok {
				cand.RedrivePolicyTargetARN = v
			}
			if v, ok := es.Detail["dlq_retry_count_in_band"].(bool); ok {
				cand.DLQRetryCountInBand = v
			}
			switch v := es.Detail["dlq_retry_count"].(type) {
			case int:
				cand.DLQRetryCount = v
			case int64:
				cand.DLQRetryCount = int(v)
			case float64:
				cand.DLQRetryCount = int(v)
			}
		}
		out = append(out, cand)
	}
	return out
}

// buildDiscoveryRecommendations walks an AI proposal plan into the
// recommendation envelopes the Recommendations tab renders. Shared by
// every cloud's generate-recommendations handler — the walk operates on
// the provider-agnostic ProposalResult, not the cloud-specific scan.
// Returns an error only if a plan step fails to marshal (should never
// happen on a struct we just produced); the caller maps it to a 500.
func buildDiscoveryRecommendations(scanID string, steps []ai.PlanStepCandidate, now time.Time) ([]recommendations.Recommendation, error) {
	recs := make([]recommendations.Recommendation, 0, len(steps))
	for i, step := range steps {
		stepJSON, err := json.Marshal(step)
		if err != nil {
			return nil, fmt.Errorf("plan step %d marshal: %w", i, err)
		}
		title := step.Name
		if title == "" {
			title = "Discovery recommendation"
		}
		detail := "AI-emitted instrumentation plan step. Run the Terraform through your IaC pipeline."
		rec := recommendations.Recommendation{
			ID:              "discovery-" + scanID + "-" + strconv.Itoa(i),
			Category:        recommendations.CategoryEmptySignal,
			Severity:        recommendations.SeverityWarn,
			Title:           title,
			Detail:          detail,
			EstSavingsBytes: 0,
			GeneratedAt:     now,
			Source: &recommendations.RecommendationSource{
				Kind:  recommendations.SourceDiscoveryScan,
				RefID: scanID,
			},
			Action: &recommendations.RecommendationAction{
				Kind:    recommendations.ActionPlan,
				Payload: stepJSON,
			},
			IaC: &recommendations.IaCSnippet{
				Format: recommendations.IaCTerraform,
				Source: step.InlineConfigSnippet,
			},
			ResourceKind:      classifyResourceKind(step.Name, step.InlineConfigSnippet),
			AffectedResources: append([]string(nil), step.AffectedResources...),
		}
		if rec.ResourceKind != "" {
			rec.Disposition = iac.DispositionFor(rec.ResourceKind)
		}
		if len(step.HCLPatch) > 0 {
			rec.HCLPatch = append([]byte(nil), step.HCLPatch...)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// firstRegion returns the first region of a scan's region list, or ""
// when empty. The discovery verdict + audit machinery is single-region
// per the slice-1 scope tuple.
func firstRegion(regions []string) string {
	if len(regions) > 0 {
		return regions[0]
	}
	return ""
}

// assembleDiscoveryVerdictBlock runs the accepted-recommendations
// few-shot assembler for a scan's scope, returning the verdict prompt
// block (the caller sets it on the DiscoveryScanContext) plus the
// accepted-PR URLs + per-state buckets for the discovery_proposal.created
// audit event. Shared by every cloud's generate-recommendations handler.
// A nil assembler or any assembler error degrades to cold-start empty:
// the proposer still runs, the prompt stays byte-identical to the
// no-verdict path, and the audit event carries an empty examples list.
func assembleDiscoveryVerdictBlock(ctx context.Context, assembler DiscoveryAcceptedRecommendationsAssembler, scopeID, region string, logger *zap.Logger) (block string, urls []string, byState map[string][]string) {
	if assembler == nil {
		return "", nil, nil
	}
	b, u, bs, err := assembler.AssembleVerdictBlockWithByState(ctx, scopeID, region)
	if err != nil {
		if logger != nil {
			logger.Warn("discovery recommendations: assemble verdict block failed; cold-start",
				zap.Error(err), zap.String("scope_id", scopeID))
		}
		return "", nil, nil
	}
	return b, u, bs
}

// emitDiscoveryProposalCreated records the discovery_proposal.created
// audit event shared by every cloud's generate-recommendations handler.
// targetID is the audit target (the connection identifier); scopeID is
// the provider scope written to the account_id payload field (AWS
// account / GCP project / Azure subscription / OCI tenancy). Safe to
// call with a nil audit service. verdict_examples_used is always present
// (empty array on cold start) so SIEM consumers can filter on it; the
// by-state map is added only when non-empty to preserve the cold-start
// byte shape.
func emitDiscoveryProposalCreated(ctx context.Context, audit services.AuditService, targetID, scopeID, region, scanID string, recCount int, urls []string, byState map[string][]string) {
	if audit == nil {
		return
	}
	examplesUsed := urls
	if examplesUsed == nil {
		examplesUsed = []string{}
	}
	payload := map[string]any{
		"scan_id":               scanID,
		"connection_id":         "",
		"account_id":            scopeID,
		"region":                region,
		"recommendation_count":  recCount,
		"verdict_examples_used": examplesUsed,
	}
	if hasAnyDiscoveryByState(byState) {
		payload["verdict_examples_used_by_state"] = byState
	}
	_ = audit.Record(ctx, services.AuditEntry{
		Actor:      "ai-proposer",
		EventType:  services.AuditEventDiscoveryProposalCreated,
		TargetType: credstore.TargetTypeCloudConnection,
		TargetID:   targetID,
		Action:     "discovery_proposal_created",
		Payload:    payload,
	})
}

func (h *DiscoveryHandlers) HandleAWSGenerateRecommendations(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingAccountID",
			Message: "Account ID path parameter is required.",
		}})
		return
	}

	if h.credStore == nil {
		// Belt-and-braces — the trampoline already 503s when credStore
		// is nil. Surfaced as 500 here for the struct-literal path.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreNotWired",
			Message:       "Squadron's credential substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.aiProposer == nil {
		// The trampoline 503s when aiProposer is nil on this route;
		// the direct-struct path lands here.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "AIProposerNotWired",
			Message: "Squadron's AI assist is not configured. Set ANTHROPIC_API_KEY and ai.enabled=true to enable discovery recommendations.",
		}})
		return
	}

	var req awsGenerateRecommendationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON. Re-run the scan and retry.",
		}})
		return
	}
	if strings.TrimSpace(req.ScanResult.ScanID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingScanID",
			Message: "scan_result.scan_id is required. Re-run the scan and retry.",
		}})
		return
	}
	if strings.TrimSpace(req.ScanResult.AccountID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingScanAccountID",
			Message: "scan_result.account_id is required.",
		}})
		return
	}
	// The URL :id and the scan_result.account_id must match. A mismatch
	// is almost always operator error (clicked the wrong tab); fail
	// loudly before the proposer runs.
	if req.ScanResult.AccountID != accountID {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "AccountIDMismatch",
			Message: "scan_result.account_id does not match the URL path account_id. Re-run the scan against the right connection and retry.",
		}})
		return
	}

	conn, err := h.credStore.GetConnection(c.Request.Context(), accountID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws generate recommendations: credstore read failed",
				zap.Error(err), zap.String("account_id", accountID))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredStoreReadFailed",
			Message:       "Squadron could not read the connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}
	if conn == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
			Code:    "ConnectionNotFound",
			Message: "No AWS connection exists with that account ID. Connect the account from the wizard first.",
		}})
		return
	}

	// Convert the scan-result wire shape into the AI context. The
	// proposer prompt reasons about categories; both lists carry the
	// same per-row fields the scanner produces.
	aiCtx := &ai.DiscoveryScanContext{
		ScanID:              req.ScanResult.ScanID,
		AccountID:           req.ScanResult.AccountID,
		Regions:             append([]string{}, req.ScanResult.Regions...),
		InstrumentedCount:   req.ScanResult.InstrumentedCount,
		UninstrumentedCount: req.ScanResult.UninstrumentedCount,
	}
	for _, ci := range req.ScanResult.Compute {
		aiCtx.ComputeInstances = append(aiCtx.ComputeInstances, ai.ComputeResourceCandidate{
			ResourceID:   ci.ResourceID,
			InstanceType: ci.InstanceType,
			Region:       ci.Region,
			OSFamily:     ci.OSFamily,
			HasOTel:      ci.HasOTel,
		})
	}
	for _, fn := range req.ScanResult.Functions {
		aiCtx.Functions = append(aiCtx.Functions, ai.FunctionResourceCandidate{
			ResourceID:   fn.ResourceID,
			Name:         fn.Name,
			Runtime:      fn.Runtime,
			Region:       fn.Region,
			HasOTelLayer: fn.HasOTelLayer,
		})
	}
	// Databases — slice 2 (v0.87). The proposer keys its PI/EM
	// reasoning off the two boolean flags carried straight from the
	// scanner. Engine + EngineVersion + InstanceClass are passed
	// through raw; the prompt body's per-engine notes (aurora-postgresql
	// inherits PI+EM, sqlserver caveats Performance Insights by
	// edition) read from these fields.
	for _, db := range req.ScanResult.Databases {
		// Database tier slice 2 (v0.89.66, #695 Stream 93) — pass
		// the Provider discriminator + the three per-cloud axis
		// flags through to the proposer. AWS rows (empty Provider
		// or "aws") fall through to the existing PI/EM logic;
		// GCP / Azure / OCI rows route to the matching kind via
		// the prompt body's per-provider rules.
		aiCtx.Databases = append(aiCtx.Databases, ai.DatabaseResourceCandidate{
			ResourceID:                 db.ResourceID,
			Engine:                     db.Engine,
			EngineVersion:              db.EngineVersion,
			InstanceClass:              db.InstanceClass,
			PerformanceInsightsEnabled: db.PerformanceInsightsEnabled,
			EnhancedMonitoringEnabled:  db.EnhancedMonitoringEnabled,
			Region:                     db.Region,
			Provider:                   db.Provider,
			QueryInsightsEnabled:       db.QueryInsightsEnabled,
			SQLInsightsDiagEnabled:     db.SQLInsightsDiagEnabled,
			DatabaseManagementEnabled:  db.DatabaseManagementEnabled,
		})
	}
	// ObjectStores + LoadBalancers — slice 3a (v0.88.0). The proposer
	// keys its single-axis instrumentation reasoning off the boolean
	// flag in each row. AccessLogsS3Bucket is passed through so the
	// proposer's ALB→S3 cross-reference rule can decide whether to
	// re-recommend (decline) or recommend a different target
	// (operator-chosen). RequestMetricsEnabled is intentionally NOT
	// in ObjectStoreCandidate — slice 3a's instrumented rule is
	// single-axis on server access logging; request-metrics is
	// operator-facing information and stays out of the prompt body.
	for _, o := range req.ScanResult.ObjectStores {
		aiCtx.ObjectStores = append(aiCtx.ObjectStores, ai.ObjectStoreCandidate{
			ResourceID:                 o.ResourceID,
			Region:                     o.Region,
			ServerAccessLoggingEnabled: o.ServerAccessLoggingEnabled,
		})
	}
	for _, l := range req.ScanResult.LoadBalancers {
		aiCtx.LoadBalancers = append(aiCtx.LoadBalancers, ai.LoadBalancerCandidate{
			ResourceID:         l.ResourceID,
			Name:               l.Name,
			Type:               l.Type,
			Scheme:             l.Scheme,
			AccessLogsEnabled:  l.AccessLogsEnabled,
			AccessLogsS3Bucket: l.AccessLogsS3Bucket,
			Region:             l.Region,
		})
	}
	// Clusters — slice 3b (v0.89.0). The proposer's composite
	// instrumented rule keys off ControlPlaneLogging (axis 1) AND
	// AddonNames (axis 2 — flattened from the wire row's
	// addons[*].name where the addon's status is ACTIVE). The
	// status-filtering happens HERE in the dispatch glue so the
	// proposer prompt body deals with a clean string list rather
	// than re-implementing the ACTIVE check. NodegroupCount /
	// FargateProfileCount are informational and not pushed into
	// the prompt body — the proposer reasons at cluster level.
	for _, c := range req.ScanResult.Clusters {
		addonNames := make([]string, 0, len(c.Addons))
		for _, a := range c.Addons {
			if !strings.EqualFold(a.Status, "ACTIVE") {
				continue
			}
			addonNames = append(addonNames, a.Name)
		}
		aiCtx.Clusters = append(aiCtx.Clusters, ai.ClusterCandidate{
			ResourceID:          c.ResourceID,
			Name:                c.Name,
			KubernetesVersion:   c.KubernetesVersion,
			ControlPlaneLogging: append([]string(nil), c.ControlPlaneLogging...),
			AddonNames:          addonNames,
			Region:              c.Region,
		})
	}
	// DynamoDB tables — slice 4 (v0.89.6). The proposer's single-
	// axis instrumented rule keys off ContributorInsightsStatus.
	// BillingMode is passed through so the prompt body's per-table
	// reasoning can hedge "enabling Contributor Insights on a
	// high-throughput PAY_PER_REQUEST table adds cost" when
	// surfacing the recommendation. The "UNKNOWN" sentinel from
	// the scanner's AccessDenied fallback is passed through verbatim
	// so the proposer can decline or hedge as appropriate.
	for _, t := range req.ScanResult.DynamoDBTables {
		aiCtx.DynamoDBTables = append(aiCtx.DynamoDBTables, ai.DynamoDBTableCandidate{
			ResourceID:                t.ResourceID,
			Name:                      t.Name,
			BillingMode:               t.BillingMode,
			ContributorInsightsStatus: t.ContributorInsightsStatus,
			Region:                    t.Region,
		})
	}
	// ECS clusters — slice 5 (v0.89.10). The proposer's single-axis
	// instrumented rule keys off ContainerInsightsStatus. Task /
	// service counts are passed through so the prompt body's
	// per-cluster reasoning can highlight high-traffic clusters
	// when surfacing the recommendation. The "UNKNOWN" sentinel
	// from the scanner's fallback is passed through verbatim so
	// the proposer can decline or hedge as appropriate.
	for _, c := range req.ScanResult.ECSClusters {
		aiCtx.ECSClusters = append(aiCtx.ECSClusters, ai.ECSClusterCandidate{
			ARN:                               c.ARN,
			Name:                              c.Name,
			Status:                            c.Status,
			ContainerInsightsStatus:           c.ContainerInsightsStatus,
			RegisteredContainerInstancesCount: c.RegisteredContainerInstancesCount,
			RunningTasksCount:                 c.RunningTasksCount,
			PendingTasksCount:                 c.PendingTasksCount,
			ActiveServicesCount:               c.ActiveServicesCount,
			Region:                            c.Region,
		})
	}

	// Event sources (v0.89.189 - the event-source -> proposer bridge).
	// Previously Result.EventSources was collected + rendered in the UI
	// but never passed to the proposer, so it could not emit any
	// event-source recommendation. Map each snapshot to an
	// EventSourceCandidate, reading the DLQ-axis Detail keys defensively
	// (Detail is map[string]any; numbers arrive as float64 after a JSON
	// round-trip, so handle int / int64 / float64).
	aiCtx.EventSources = mapEventSourceCandidates(req.ScanResult.EventSources)

	// v0.89.28 (#643 slice 1) → v0.89.36 (#655 Stream 53, #531 slice
	// 2 chunk 3) — populate the verdict few-shot block before the
	// proposer call. AssembleVerdictBlock returns ("", nil, nil) on
	// cold start / opt-out / recency-window empty; in that case the
	// prompt is byte-for-byte unchanged from pre-v0.89.28. A bridge-
	// side error is non-fatal: log and proceed with no block so the
	// proposer still produces output. The unioned `acceptedURLs`
	// flow into the audit payload's verdict_examples_used field
	// (chunk 4 will fan them out into per-state buckets via
	// verdict_examples_used_by_state).
	var acceptedURLs []string
	var acceptedURLsByState map[string][]string
	if h.acceptedAssembler != nil {
		// Single-region scope tuple. Slice 1 ships single-region
		// scans (req.ScanResult.Regions[0]); multi-region scans are a
		// later slice and would call the assembler once per region.
		region := ""
		if len(req.ScanResult.Regions) > 0 {
			region = req.ScanResult.Regions[0]
		}
		// v0.89.37 (#657 Stream 55, #531 slice 2 chunk 6) — call the
		// by-state extension so the discovery_proposal.created audit
		// row can carry verdict_examples_used_by_state alongside the
		// flat verdict_examples_used array. On cold start
		// urlsByState is empty (nil) and the audit emit omits the
		// new field to preserve v0.89.28 byte shape.
		block, urls, byState, aErr := h.acceptedAssembler.AssembleVerdictBlockWithByState(
			c.Request.Context(), req.ScanResult.AccountID, region,
		)
		if aErr != nil {
			if h.logger != nil {
				h.logger.Warn("aws generate recommendations: assemble verdict block failed; cold-start",
					zap.Error(aErr), zap.String("account_id", accountID))
			}
		} else {
			aiCtx.VerdictBlock = block
			acceptedURLs = urls
			acceptedURLsByState = byState
		}
	}

	// v0.89.209 async: the proposer call (sonnet-4-6 @ 8192 tokens) can run
	// 30s-120s+, past any sane HTTP timeout. Kick it off in a background job
	// and return 202 + a job_id the UI polls. Everything below runs in the
	// job on a context detached from this request, so returning does not
	// cancel the proposer.
	job := h.recJobs.Create("aws", accountID)
	h.recJobs.Run(job.ID, func(ctx context.Context) (json.RawMessage, *scanner.HumanizedError, int) {
		result, err := h.aiProposer.ProposeFromDiscoveryScan(ctx, aiCtx)
		if err != nil {
			if h.logger != nil {
				h.logger.Warn("aws generate recommendations: proposer call failed",
					zap.Error(err), zap.String("account_id", accountID), zap.String("scan_id", aiCtx.ScanID))
			}
			return nil, &scanner.HumanizedError{
				Code:    "ProposerCallFailed",
				Message: "Squadron's AI proposer failed: " + err.Error(),
			}, http.StatusInternalServerError
		}

		if result.Declined {
			// Surface the model's reason; no audit event for
			// recommendations_generated (nothing was generated). An empty
			// Recommendations array — never null — so the UI's branch on
			// .length stays simple.
			return marshalRecResult(awsGenerateRecommendationsResponse{
				Declined:        true,
				Reason:          result.Reason,
				Recommendations: []recommendations.Recommendation{},
			})
		}

		// Walk the plan-kind result into one Recommendation per step. Each
		// step's Terraform lands in the typed IaC field (the v0.85 Stream
		// 2B addition); the Action payload carries the step JSON for the
		// UI's preview flow.
		now := time.Now().UTC()
		recs, err := buildDiscoveryRecommendations(req.ScanResult.ScanID, result.Plan.Steps, now)
		if err != nil {
			if h.logger != nil {
				h.logger.Error("aws generate recommendations: plan step marshal failed", zap.Error(err))
			}
			return nil, &scanner.HumanizedError{
				Code:    "PlanStepMarshalFailed",
				Message: "Squadron could not encode the plan step. The error has been logged.",
			}, http.StatusInternalServerError
		}

		// Audit event. Payload deliberately omits the Terraform content —
		// audit rows shouldn't grow with snippet size. step_count +
		// scan_id + token metering are what an auditor needs to
		// reconstruct "what was generated and how much did it cost".
		if h.auditService != nil {
			_ = h.auditService.Record(ctx, services.AuditEntry{
				Actor:      "system",
				EventType:  "discovery.aws.recommendations_generated",
				TargetType: credstore.TargetTypeCloudConnection,
				TargetID:   accountID,
				Action:     "recommendations_generated",
				Payload: map[string]any{
					"account_id":  accountID,
					"scan_id":     req.ScanResult.ScanID,
					"step_count":  len(recs),
					"tokens_in":   result.TokensIn,
					"tokens_out":  result.TokensOut,
					"model":       result.Model,
					"recorded_at": now,
				},
			})

			// v0.89.28 (#643 slice 1) — discovery_proposal.created event.
			// Mirrors the cost-spike side's proposal.created posture but
			// the verdict_examples_used field carries PR URLs (the
			// identifying handle for accepted discovery recommendations,
			// per §11 Q5 of the spec) rather than rollout IDs. ALWAYS
			// present (never omitted) — empty array on cold start so
			// SIEM consumers can filter on the empty slice.
			examplesUsed := acceptedURLs
			if examplesUsed == nil {
				examplesUsed = []string{}
			}
			region := ""
			if len(req.ScanResult.Regions) > 0 {
				region = req.ScanResult.Regions[0]
			}
			discoveryPayload := map[string]any{
				"scan_id":               req.ScanResult.ScanID,
				"connection_id":         "", // slice 1: handler doesn't see IaC connection_id
				"account_id":            accountID,
				"region":                region,
				"recommendation_count":  len(recs),
				"verdict_examples_used": examplesUsed,
			}
			// v0.89.37 (#657 Stream 55, #531 slice 2 chunk 6) — extended
			// payload with verdict_examples_used_by_state. Emitted only
			// when at least one bucket is non-empty so cold-start /
			// opt-out / recency-empty audit rows stay byte-for-byte
			// identical to the v0.89.28 shape (the existing flat
			// verdict_examples_used: [] field remains the cold-start
			// signal SIEM consumers already filter on). Spec §8 (c).
			if hasAnyDiscoveryByState(acceptedURLsByState) {
				discoveryPayload["verdict_examples_used_by_state"] = acceptedURLsByState
			}
			_ = h.auditService.Record(ctx, services.AuditEntry{
				Actor:      "ai-proposer",
				EventType:  services.AuditEventDiscoveryProposalCreated,
				TargetType: credstore.TargetTypeCloudConnection,
				TargetID:   accountID,
				Action:     "discovery_proposal_created",
				Payload:    discoveryPayload,
			})
		}

		return marshalRecResult(awsGenerateRecommendationsResponse{
			Declined:        false,
			Reasoning:       result.Reasoning,
			Recommendations: recs,
		})
	})

	c.JSON(http.StatusAccepted, recommendationJobAcceptedResponse{
		JobID:  job.ID,
		Status: string(RecJobPending),
	})
}

// hasAnyDiscoveryByState returns true when the supplied per-state
// URL bucket map has any non-empty bucket. Used to gate emission of
// the verdict_examples_used_by_state field on
// discovery_proposal.created — cold-start / opt-out / recency-empty
// rows omit the field so the v0.89.28 audit payload shape is
// preserved byte-for-byte for SIEM consumers that already filter on
// the flat verdict_examples_used: [] cold-start signal. v0.89.37
// (#657 Stream 55, #531 slice 2 chunk 6).
func hasAnyDiscoveryByState(m map[string][]string) bool {
	for _, urls := range m {
		if len(urls) > 0 {
			return true
		}
	}
	return false
}

// classifyResourceKind maps a discovery plan step to one of the
// slice-1 placement-map resource_kind strings. Returns "" when no
// match — the Recommendations tab treats that as "Open PR not
// available, copy-only".
//
// The classifier is intentionally snippet-first, name-second. The
// Terraform resource type the proposer emits is a stable signal
// (the prompt body in proposer_discovery_prompt.go pins these
// resource shapes — aws_lambda_function for Lambda, aws_ssm_*
// for EC2 ADOT, aws_db_instance for RDS PI/EM, etc.). The step
// name is human prose and could drift across prompt revisions;
// the snippet's resource type is the schema-stable wire artifact.
//
// EKS classification: the proposer emits a SINGLE step per
// uncovered cluster covering BOTH axes (control-plane logging +
// observability addon). The placement map has two rows for the
// two axes. Slice-1 classification picks eks-observability-addon
// when the snippet creates an aws_eks_addon resource — the more
// specific lever. eks-cluster-logging is the fallback when the
// snippet only touches aws_eks_cluster.enabled_cluster_log_types.
// The PR builder appends to whichever placement file matches; the
// operator's single PR review covers both axes regardless.
func classifyResourceKind(stepName, snippet string) string {
	body := snippet
	if len(body) > 4096 {
		// The classifier only needs the first few KB — a long snippet
		// won't add resource types beyond the ones in the first block.
		body = body[:4096]
	}
	lower := strings.ToLower(body)
	nameLower := strings.ToLower(stepName)

	// EKS is checked first because the addon snippet may also touch
	// the cluster resource for the second axis; the addon kind wins.
	switch {
	case strings.Contains(lower, "aws_eks_addon"):
		return "eks-observability-addon"
	case strings.Contains(lower, "aws_eks_cluster"):
		return "eks-cluster-logging"
	case strings.Contains(lower, "aws_dynamodb_contributor_insights"):
		return "dynamodb-contributor-insights"
	case strings.Contains(lower, "aws_ecs_cluster"):
		return "ecs-container-insights"
	case strings.Contains(lower, "aws_lambda_function"),
		strings.Contains(lower, "aws_lambda_layer_version"):
		return "lambda-otel-layer"
	case strings.Contains(lower, "aws_ssm_association"),
		strings.Contains(lower, "aws_ssm_document"),
		strings.Contains(lower, "user_data") && strings.Contains(nameLower, "ec2"):
		return "ec2-otel-layer"
	case strings.Contains(lower, "aws_db_instance"),
		strings.Contains(lower, "performance_insights"),
		strings.Contains(lower, "monitoring_interval"):
		return "rds-pi-em"
	case strings.Contains(lower, "aws_s3_bucket_logging"),
		strings.Contains(lower, "aws_s3_bucket_server_side_logging"):
		return "s3-access-logging"
	case strings.Contains(lower, "aws_lb") && strings.Contains(lower, "access_logs"),
		strings.Contains(lower, "aws_alb"):
		return "alb-access-logs"
	}

	// Step-name fallback for prompts that emit Terraform we don't
	// recognize. Less reliable than the snippet match but keeps the
	// UI's Open PR available when the proposer is verbose about the
	// category in the step name.
	switch {
	case strings.Contains(nameLower, "lambda"):
		return "lambda-otel-layer"
	case strings.Contains(nameLower, "ec2"):
		return "ec2-otel-layer"
	case strings.Contains(nameLower, "rds"), strings.Contains(nameLower, "performance insights"):
		return "rds-pi-em"
	case strings.Contains(nameLower, "s3") && strings.Contains(nameLower, "access log"):
		return "s3-access-logging"
	case strings.Contains(nameLower, "alb"), strings.Contains(nameLower, "nlb"),
		strings.Contains(nameLower, "load balancer"):
		return "alb-access-logs"
	case strings.Contains(nameLower, "eks") && strings.Contains(nameLower, "addon"):
		return "eks-observability-addon"
	case strings.Contains(nameLower, "eks"):
		return "eks-cluster-logging"
	case strings.Contains(nameLower, "dynamodb"),
		strings.Contains(nameLower, "contributor insights"):
		return "dynamodb-contributor-insights"
	case strings.Contains(nameLower, "ecs"),
		strings.Contains(nameLower, "container insights"):
		return "ecs-container-insights"
	}

	return ""
}

// countInstrumentedDynamoDB tallies the slice 4 (v0.89.6) tables in
// the scan result whose ContributorInsightsStatus == "ENABLED" — the
// single-axis instrumented rule. Used by the scan_completed audit
// payload's instrumented_dynamodb_count field so SIEM forwarders
// and the proposer's future scan-history loop don't have to
// recompute from the row list.
func countInstrumentedDynamoDB(tables []scanner.DynamoDBTableSnapshot) int {
	n := 0
	for _, t := range tables {
		if t.IsInstrumented() {
			n++
		}
	}
	return n
}

// countInstrumentedECS tallies the slice 5 (v0.89.10) ECS clusters
// in the scan result whose containerInsights setting value is
// "enabled" (case-insensitive — see scanner.ECSClusterSnapshot's
// IsInstrumented predicate). The single-axis instrumented rule.
// Used by the scan_completed audit payload's instrumented_ecs_count
// field so SIEM forwarders and the proposer's future scan-history
// loop don't have to recompute from the row list.
func countInstrumentedECS(clusters []scanner.ECSClusterSnapshot) int {
	n := 0
	for _, c := range clusters {
		if c.IsInstrumented() {
			n++
		}
	}
	return n
}

// --- HandleAWSRecommendationExclude (v0.89.37 #656 Stream 54) -------
//
// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — operator-set
// exclusion endpoint for discovery recommendations. POSTed by the
// Recommendations tab's "Don't propose this again" button. Inserts
// or updates a row in iac_recommendation_verdicts and emits an audit
// event on state transitions.

// awsRecommendationExcludeRequest is the JSON wire shape the
// Recommendations tab POSTs when the operator clicks the "Don't
// propose this again" affordance. ResourceID is optional — empty
// scopes the exclusion to the entire kind at scope; non-empty scopes
// it to a single resource (the §11 Q4 distinction the prompt
// renderer surfaces with different instruction text).
//
// Excluded is the desired final state. true on click; false on
// un-click (the operator changed their mind). The handler treats the
// boolean as authoritative and routes through
// SetRecommendationExclusion to upsert.
type awsRecommendationExcludeRequest struct {
	RecommendationID   string `json:"recommendation_id"`
	ConnectionID       string `json:"connection_id"`
	AccountID          string `json:"account_id"`
	Region             string `json:"region"`
	RecommendationKind string `json:"recommendation_kind"`
	ResourceID         string `json:"resource_id,omitempty"`
	Excluded           bool   `json:"excluded"`
}

// awsRecommendationExcludeResponse echoes the persisted state back so
// the UI can confirm the toggle landed. Excluded is the canonical
// (post-upsert) value; ExcludedAt + ExcludedBy carry the stamp the
// store recorded on a transition to excluded=true. On excluded=false
// the response omits the timestamp and actor (the row stays around
// with cleared stamps).
type awsRecommendationExcludeResponse struct {
	RecommendationID string    `json:"recommendation_id"`
	Excluded         bool      `json:"excluded"`
	ExcludedAt       time.Time `json:"excluded_at,omitempty"`
	ExcludedBy       string    `json:"excluded_by,omitempty"`
}

// HandleAWSRecommendationExclude — POST
// /api/v1/discovery/aws/recommendations/exclude.
//
// Flow:
//  1. Parse + validate request body. 400 on malformed JSON or missing
//     required fields (recommendation_id, connection_id, account_id,
//     region, recommendation_kind). The excluded field is required to
//     be present; the JSON binder defaults missing booleans to false,
//     so the explicit `excluded` field is the authoritative signal.
//  2. Build the ExcludedRecommendation projection with ExcludedAt=now
//     and ExcludedBy=authenticated actor. The store overrides these on
//     transition direction (clear on true→false; preserve on no-op).
//  3. Call SetRecommendationExclusion → get prevExcluded.
//  4. Emit the audit event on transitions only (excluded on
//     false→true; exclude_cleared on true→false). No-op toggles
//     produce no audit row.
//  5. Return 200 with the post-upsert state.
//
// The handler returns 503 when no exclusion store is wired and 500
// when SetRecommendationExclusion errors — both surfaced as
// HumanizedError so the UI renders a clear "retry" affordance.
func (h *DiscoveryHandlers) HandleAWSRecommendationExclude(c *gin.Context) {
	if h.exclusionStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "ExclusionStoreNotWired",
			Message: "Squadron's exclusion store is not configured. The application backend must be wired before this affordance is available.",
		}})
		return
	}
	var req awsRecommendationExcludeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}
	if strings.TrimSpace(req.RecommendationID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingRecommendationID",
			Message: "recommendation_id is required.",
		}})
		return
	}
	if strings.TrimSpace(req.ConnectionID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "connection_id is required.",
		}})
		return
	}
	if strings.TrimSpace(req.AccountID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingAccountID",
			Message: "account_id is required.",
		}})
		return
	}
	if strings.TrimSpace(req.Region) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingRegion",
			Message: "region is required.",
		}})
		return
	}
	if strings.TrimSpace(req.RecommendationKind) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingRecommendationKind",
			Message: "recommendation_kind is required.",
		}})
		return
	}

	actor := actorFromContext(c)
	now := time.Now().UTC()
	rec := types.ExcludedRecommendation{
		RecommendationID:   req.RecommendationID,
		ConnectionID:       req.ConnectionID,
		AccountID:          req.AccountID,
		Region:             req.Region,
		RecommendationKind: req.RecommendationKind,
		ResourceID:         req.ResourceID,
		ExcludedAt:         now,
		ExcludedBy:         actor,
	}
	prevExcluded, err := h.exclusionStore.SetRecommendationExclusion(
		c.Request.Context(), rec, req.Excluded,
	)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws recommendation exclude: store write failed",
				zap.Error(err),
				zap.String("recommendation_id", req.RecommendationID))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "ExclusionStoreWriteFailed",
			Message: "Squadron could not persist the exclusion. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Audit emit on transitions only. The handler is authoritative on
	// the transition signal — the store's prevExcluded carries the
	// canonical pre-call value, including the false-on-insert case.
	if h.auditService != nil && prevExcluded != req.Excluded {
		payload := map[string]any{
			"recommendation_id":   req.RecommendationID,
			"connection_id":       req.ConnectionID,
			"account_id":          req.AccountID,
			"region":              req.Region,
			"recommendation_kind": req.RecommendationKind,
		}
		if req.ResourceID != "" {
			payload["resource_id"] = req.ResourceID
		}
		var eventType, action, actorKey string
		if req.Excluded {
			eventType = services.AuditEventDiscoveryRecommendationExcluded
			action = "excluded"
			actorKey = "excluded_by"
		} else {
			eventType = services.AuditEventDiscoveryRecommendationExcludeCleared
			action = "exclude_cleared"
			actorKey = "cleared_by"
		}
		payload[actorKey] = actor
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      actor,
			EventType:  eventType,
			TargetType: services.AuditTargetIaCRecommendation,
			TargetID:   req.RecommendationID,
			Action:     action,
			Payload:    payload,
		})
	}

	// v0.89.44 (#665 Stream 63, slice 1 chunk 4 of the GitHub Checks
	// API back-signal arc). Follow up on the
	// discovery_recommendation.excluded emit by PATCHing the in-flight
	// check run (if any) to conclusion=neutral so the operator who's
	// reading the PR review surface sees Squadron's reasoning
	// up-to-date without having to bounce back to Squadron's UI. Only
	// fires on the false → true (Excluded) transition; the cleared and
	// no-op paths leave the check run wherever it was (the merge /
	// close webhook will reconcile if the PR moves). See design doc §7
	// + §10 contract item 7 + §12 acceptance test 5.
	if req.Excluded && prevExcluded != req.Excluded {
		h.maybeUpdateCheckRunOnExclude(c.Request.Context(), &req, actor)
	}

	resp := awsRecommendationExcludeResponse{
		RecommendationID: req.RecommendationID,
		Excluded:         req.Excluded,
	}
	if req.Excluded {
		resp.ExcludedAt = now
		resp.ExcludedBy = actor
	}
	c.JSON(http.StatusOK, resp)
}

// maybeUpdateCheckRunOnExclude — v0.89.44 (#665 Stream 63, slice 1
// chunk 4) — PATCHes the in-flight check run for recommendationID to
// conclusion=neutral with the operator-exclude summary, and emits the
// iac.check_run.updated audit event with the transition payload. The
// helper is fail-open: any missing wiring (nil checksClient, nil
// checkRunStore, empty PAT), any short-circuit condition (no row,
// completed status), any GitHub failure path all degrade silently —
// the operator's excluded audit event already fired upstream.
//
// Short-circuit conditions (all silent):
//   - h.checksClient == nil: chunk-4 Checks API not wired on this
//     deployment.
//   - h.checkRunStore == nil: chunk-4 storage surface not wired.
//   - h.checksPAT == "": PAT not configured.
//   - GetCheckRunForRecommendation returns exists=false: no row for
//     this recommendation (chunk-2 bridge never opened a PR for it,
//     or the row was pruned).
//   - status == "completed": the PR has already been merged or
//     closed; the conclusion is final. PATCHing to neutral would
//     overwrite the success / failure state the merge / close
//     webhook already PATCHed in. Per design doc §7: "only fires if
//     the underlying PR is still open."
//
// On UpdateCheckRun success: emit iac.check_run.updated with
// transition payload (previous_status + previous_conclusion +
// new_status + new_conclusion). Persist the new state via
// SetCheckRunForRecommendation so the next read sees completed +
// neutral. On UpdateCheckRun failure: emit iac.check_run.failed with
// the structured error_kind discriminator.
func (h *DiscoveryHandlers) maybeUpdateCheckRunOnExclude(
	ctx context.Context,
	req *awsRecommendationExcludeRequest,
	actor string,
) {
	if h.checksClient == nil || h.checkRunStore == nil {
		return
	}
	if strings.TrimSpace(h.checksPAT) == "" {
		return
	}
	if strings.TrimSpace(req.RecommendationID) == "" {
		return
	}

	ref, prevStatus, prevConclusion, exists, err := h.checkRunStore.GetCheckRunForRecommendation(ctx, req.RecommendationID)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("aws recommendation exclude: check-run lookup failed",
				zap.Error(err),
				zap.String("recommendation_id", req.RecommendationID))
		}
		return
	}
	if !exists || ref.CheckID == 0 {
		// No check run was ever opened for this recommendation —
		// nothing to PATCH. Fast path for deployments that haven't
		// upgraded chunk-2's bridge wiring yet.
		return
	}
	if prevStatus == iacgithub.CheckRunStatusCompleted {
		// PR has already been merged or closed; the conclusion is
		// final. Comment per design doc §7: only the merge / close
		// webhook transitions a completed run; the operator-exclude
		// neutral arrow is only valid from in_progress.
		return
	}

	in := checkrunprompt.SummaryInput{
		RecommendationKind: req.RecommendationKind,
		// RecommendationReason is intentionally empty on the exclusion
		// PATCH — the operator's intent is "don't propose this again,"
		// not "here's the original reasoning a second time." The
		// summary's sanitizeReasoning emits a placeholder when empty.
		AccountID:        req.AccountID,
		Region:           req.Region,
		ConnectionID:     req.ConnectionID,
		PRURL:            "",
		RecommendationID: req.RecommendationID,
		SquadronHost:     h.squadronHost,
	}
	title, summary := checkrunprompt.ComposeUpdateSummary(in, iacgithub.CheckRunConclusionNeutral)

	updateReq := iacgithub.CheckRunUpdate{
		Ref: iacgithub.CheckRunRef{
			Owner:   ref.Owner,
			Repo:    ref.Repo,
			CheckID: ref.CheckID,
			HeadSHA: ref.HeadSHA,
		},
		Status:      iacgithub.CheckRunStatusCompleted,
		Conclusion:  iacgithub.CheckRunConclusionNeutral,
		CompletedAt: time.Now().UTC(),
		Output: iacgithub.CheckRunOutput{
			Title:   title,
			Summary: summary,
		},
	}
	if err := h.checksClient.UpdateCheckRun(ctx, h.checksPAT, updateReq); err != nil {
		h.emitDiscoveryCheckRunFailedAudit(ctx, req, ref, err)
		return
	}

	// Persist the new state.
	rec := types.ExcludedRecommendation{
		RecommendationID:   req.RecommendationID,
		ConnectionID:       req.ConnectionID,
		AccountID:          req.AccountID,
		Region:             req.Region,
		RecommendationKind: req.RecommendationKind,
		ResourceID:         req.ResourceID,
		ExcludedAt:         time.Now().UTC(),
		ExcludedBy:         actor,
	}
	if werr := h.checkRunStore.SetCheckRunForRecommendation(
		ctx, rec,
		types.CheckRunRef{Owner: ref.Owner, Repo: ref.Repo, CheckID: ref.CheckID, HeadSHA: ref.HeadSHA},
		iacgithub.CheckRunStatusCompleted,
		iacgithub.CheckRunConclusionNeutral,
	); werr != nil && h.logger != nil {
		h.logger.Warn("aws recommendation exclude: check-run storage write failed (fail-open)",
			zap.Error(werr),
			zap.Int64("check_run_id", ref.CheckID))
	}

	h.emitDiscoveryCheckRunUpdatedAudit(ctx, req, ref, prevStatus, prevConclusion)
}

// emitDiscoveryCheckRunUpdatedAudit records iac.check_run.updated for
// the chunk-4 exclusion path. Mirrors the chunk-2/3 emit shape so the
// SIEM forwarder + humanizer see a single uniform schema across all
// three transition sources.
func (h *DiscoveryHandlers) emitDiscoveryCheckRunUpdatedAudit(
	ctx context.Context,
	req *awsRecommendationExcludeRequest,
	ref types.CheckRunRef,
	prevStatus, prevConclusion string,
) {
	if h.auditService == nil {
		return
	}
	prURL := ""
	if ref.Owner != "" && ref.Repo != "" {
		// Reconstruct a humanizer-friendly PR-coordinates URL even
		// though we don't have the PR number on this path — the
		// humanizer's title falls back to a kindless form when the
		// "/pull/<n>" suffix is missing. The owner/repo half is the
		// audit-correlation value SIEM consumers join on.
		prURL = "https://github.com/" + ref.Owner + "/" + ref.Repo
	}
	payload := map[string]any{
		"connection_id":       req.ConnectionID,
		"recommendation_id":   req.RecommendationID,
		"recommendation_kind": req.RecommendationKind,
		"account_id":          req.AccountID,
		"region":              req.Region,
		"pr_url":              prURL,
		"head_sha":            ref.HeadSHA,
		"check_run_id":        ref.CheckID,
		"owner":               ref.Owner,
		"repo":                ref.Repo,
		"previous_status":     prevStatus,
		"previous_conclusion": prevConclusion,
		"new_status":          iacgithub.CheckRunStatusCompleted,
		"new_conclusion":      iacgithub.CheckRunConclusionNeutral,
		"actor":               services.AuditActorSystem,
		"recorded_at":         time.Now().UTC(),
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventIaCCheckRunUpdated,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   req.ConnectionID,
		Action:     "check_run_updated",
		Payload:    payload,
	})
}

// emitDiscoveryCheckRunFailedAudit records iac.check_run.failed when
// the UpdateCheckRun call fails on the chunk-4 exclusion path. The
// structured error_kind discriminator drives SIEM dashboards and the
// humanizer's fix-it copy.
func (h *DiscoveryHandlers) emitDiscoveryCheckRunFailedAudit(
	ctx context.Context,
	req *awsRecommendationExcludeRequest,
	ref types.CheckRunRef,
	err error,
) {
	if h.auditService == nil {
		return
	}
	errKind := iacgithub.CheckRunErrorKindNetwork
	httpStatus := 0
	msg := err.Error()
	var cre *iacgithub.CheckRunError
	if errors.As(err, &cre) {
		errKind = cre.Kind
		httpStatus = cre.Status
		msg = cre.Message
	}
	prURL := ""
	if ref.Owner != "" && ref.Repo != "" {
		prURL = "https://github.com/" + ref.Owner + "/" + ref.Repo
	}
	payload := map[string]any{
		"connection_id":       req.ConnectionID,
		"recommendation_id":   req.RecommendationID,
		"recommendation_kind": req.RecommendationKind,
		"account_id":          req.AccountID,
		"region":              req.Region,
		"pr_url":              prURL,
		"head_sha":            ref.HeadSHA,
		"check_run_id":        ref.CheckID,
		"owner":               ref.Owner,
		"repo":                ref.Repo,
		"error_kind":          errKind,
		"http_status":         httpStatus,
		"error_message":       msg,
		"actor":               services.AuditActorSystem,
		"recorded_at":         time.Now().UTC(),
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventIaCCheckRunFailed,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   req.ConnectionID,
		Action:     "check_run_failed",
		Payload:    payload,
	})
	if h.logger != nil {
		h.logger.Info("aws recommendation exclude: check run update failed (fail-open)",
			zap.String("error_kind", errKind),
			zap.Int("http_status", httpStatus),
			zap.String("recommendation_id", req.RecommendationID))
	}
}

// --- HandleAWSRecommendationListExcluded (v0.89.40 #660 Stream 58) ---
//
// v0.89.40 (#660 Stream 58, #531 slice 2 chunk 5 follow-on) — read
// surface for the operator-set exclusion table. The discovery
// Recommendations tab GETs this on mount to hydrate its excludedSet
// from the persisted iac_recommendation_verdicts table so the
// Excluded badges survive a page refresh. Chunk 5 shipped the POST
// half of this loop and an explicit TODO acknowledging the UI lost
// state on refresh; this closes that gap without changing chunk 4's
// schema or chunk 5's toggle behavior.

// awsRecommendationListExcludedRow is the per-row JSON shape the
// list endpoint emits. Mirrors types.ExcludedRecommendation but only
// surfaces fields the UI needs to identify a recommendation by ID +
// kind + resource (the audit timeline is the authoritative log for
// who-set-when across all rows). ExcludedAt is ISO-8601 string on
// the wire so the UI can format it consistently with the other
// timestamps it renders.
type awsRecommendationListExcludedRow struct {
	RecommendationID   string    `json:"recommendation_id"`
	RecommendationKind string    `json:"recommendation_kind"`
	ResourceID         string    `json:"resource_id,omitempty"`
	ExcludedAt         time.Time `json:"excluded_at"`
	ExcludedBy         string    `json:"excluded_by"`
}

// awsRecommendationListExcludedResponse wraps the rows under a
// single `excluded` key. Always emits an array (never null) so the
// UI's empty-state branch is a single `.length === 0` check —
// matching the existing list-endpoint posture across discovery.
type awsRecommendationListExcludedResponse struct {
	Excluded []awsRecommendationListExcludedRow `json:"excluded"`
}

// defaultListExcludedLimit caps the result set when the operator
// doesn't pass an explicit limit. 100 mirrors the storage method's
// default and the discovery bridge's typical sweep size — large
// enough to hydrate a normal Recommendations tab in a single
// round trip, small enough that a runaway scope query can't return
// the entire substrate table.
const defaultListExcludedLimit = 100

// maxListExcludedLimit is the hard ceiling regardless of operator
// request. Matches the storage method's clamp so the handler's
// behavior is identical to what the bridge sees.
const maxListExcludedLimit = 1000

// HandleAWSRecommendationListExcluded — GET
// /api/v1/discovery/aws/recommendations/excluded.
//
// Flow:
//  1. Parse query params: connection_id, account_id, region (all
//     required → 400 on missing); optional limit (default 100).
//  2. Call store.ListExcludedRecommendations(ctx, ...). 500 on
//     storage error.
//  3. Walk the rows into the wire shape and return 200 with an array
//     (possibly empty) under the `excluded` key.
//
// No audit emission — this is a read endpoint. agents:read scope on
// the route (set at registration in server.go).
//
// The handler returns 503 when no exclusion store is wired (matches
// the POST sibling's posture so AI-disabled / no-substrate
// deployments degrade consistently).
func (h *DiscoveryHandlers) HandleAWSRecommendationListExcluded(c *gin.Context) {
	if h.exclusionStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "ExclusionStoreNotWired",
			Message: "Squadron's exclusion store is not configured. The application backend must be wired before this affordance is available.",
		}})
		return
	}
	connectionID := strings.TrimSpace(c.Query("connection_id"))
	accountID := strings.TrimSpace(c.Query("account_id"))
	region := strings.TrimSpace(c.Query("region"))
	if connectionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "connection_id query parameter is required.",
		}})
		return
	}
	if accountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingAccountID",
			Message: "account_id query parameter is required.",
		}})
		return
	}
	if region == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingRegion",
			Message: "region query parameter is required.",
		}})
		return
	}

	limit := defaultListExcludedLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > maxListExcludedLimit {
		limit = maxListExcludedLimit
	}

	rows, err := h.exclusionStore.ListExcludedRecommendations(
		c.Request.Context(), connectionID, accountID, region, limit,
	)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws recommendation list excluded: store read failed",
				zap.Error(err),
				zap.String("connection_id", connectionID),
				zap.String("account_id", accountID),
				zap.String("region", region))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "ExclusionStoreReadFailed",
			Message: "Squadron could not read the exclusion list. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Always emit an array — never null — so the UI's empty-state
	// branch is a single .length check.
	out := make([]awsRecommendationListExcludedRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, awsRecommendationListExcludedRow{
			RecommendationID:   r.RecommendationID,
			RecommendationKind: r.RecommendationKind,
			ResourceID:         r.ResourceID,
			ExcludedAt:         r.ExcludedAt,
			ExcludedBy:         r.ExcludedBy,
		})
	}
	c.JSON(http.StatusOK, awsRecommendationListExcludedResponse{Excluded: out})
}
