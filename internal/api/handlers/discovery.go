// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
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
	credStore         credstore.Store
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
	logger     *zap.Logger
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

	vr, err := validator.Validate(c.Request.Context(), conn)
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
	vr, err := validator.Validate(c.Request.Context(), transientConn)
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
type awsConnectionRow struct {
	AccountID   string    `json:"account_id"`
	DisplayName string    `json:"display_name"`
	Regions     []string  `json:"regions"`
	CreatedAt   time.Time `json:"created_at"`
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
			AccountID:   conn.AccountID,
			DisplayName: conn.DisplayName,
			Regions:     conn.Regions,
			CreatedAt:   conn.CreatedAt,
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
}

// awsScanResponse is the snake_case wire shape the React Inventory tab
// consumes. scanner.Result is defined without JSON tags (the scanner
// package is provider-agnostic and the handler owns the wire contract),
// so we walk it once into a tagged struct rather than emit the Go
// field names verbatim.
type awsScanResponse struct {
	ScanID              string                  `json:"scan_id"`
	ScanStartedAt       time.Time               `json:"scan_started_at"`
	ScanCompletedAt     time.Time               `json:"scan_completed_at"`
	AccountID           string                  `json:"account_id"`
	Provider            string                  `json:"provider"`
	Regions             []string                `json:"regions"`
	Compute             []awsComputeInstanceRow `json:"compute"`
	Functions           []awsFunctionRuntimeRow `json:"functions"`
	InstrumentedCount   int                     `json:"instrumented_count"`
	UninstrumentedCount int                     `json:"uninstrumented_count"`
	Partial             bool                    `json:"partial"`
	PartialReason       string                  `json:"partial_reason,omitempty"`
}

type awsComputeInstanceRow struct {
	ResourceID   string            `json:"resource_id"`
	InstanceType string            `json:"instance_type"`
	Tags         map[string]string `json:"tags"`
	HasOTel      bool              `json:"has_otel"`
	OSFamily     string            `json:"os_family"`
	Region       string            `json:"region"`
}

type awsFunctionRuntimeRow struct {
	ResourceID   string `json:"resource_id"`
	Name         string `json:"name"`
	Runtime      string `json:"runtime"`
	HasOTelLayer bool   `json:"has_otel_layer"`
	Region       string `json:"region"`
}

// marshalScanResult walks the scanner.Result into the snake_case wire
// shape. Empty slices stay empty (never null) so the UI's empty-state
// rendering keys off .length === 0 rather than nil-checking.
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
		InstrumentedCount:   r.InstrumentedCount,
		UninstrumentedCount: r.UninstrumentedCount,
		Partial:             r.Partial,
		PartialReason:       r.PartialReason,
	}
	for _, ci := range r.Compute {
		out.Compute = append(out.Compute, awsComputeInstanceRow{
			ResourceID:   ci.ResourceID,
			InstanceType: ci.InstanceType,
			Tags:         ci.Tags,
			HasOTel:      ci.HasOTel,
			OSFamily:     ci.OSFamily,
			Region:       ci.Region,
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
	return out
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
//     compute_count, function_count, instrumented_count,
//     uninstrumented_count, and the partial flag in the payload
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

	// Body is optional — the empty-body path falls back to the
	// connection's stored Regions. Parse failures fall through to the
	// empty-body branch rather than 400ing; the operator's intent is
	// "scan whatever's configured" and we honor that even when the
	// browser sent no payload.
	var req awsRunScanRequest
	_ = c.ShouldBindJSON(&req)

	conn, err := h.credStore.GetConnection(c.Request.Context(), accountID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws run scan: credstore read failed", zap.Error(err), zap.String("account_id", accountID))
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

	// Resolve the regions to scan. Empty request body falls back to
	// the connection's stored list — slice 1 ships single-entry lists,
	// slice 3 will iterate.
	regions := req.Regions
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
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      "system",
			EventType:  "discovery.aws.scan_started",
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   accountID,
			Action:     "scan_started",
			Payload: map[string]any{
				"account_id":  accountID,
				"regions":     regions,
				"recorded_at": time.Now().UTC(),
			},
		})
	}

	awsScanner, err := h.awsScannerFor(conn)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("aws run scan: scanner construction failed", zap.Error(err), zap.String("account_id", accountID))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "ScannerConstructFailed",
			Message:       "Squadron could not decrypt the connection credentials. Re-validate from the wizard.",
			SuggestedStep: "validate",
		}})
		return
	}

	result, err := awsScanner.Scan(c.Request.Context(), conn, regions)
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "ScannerInternal",
			Message:       "Scan failed unexpectedly: " + err.Error(),
			SuggestedStep: "validate",
		}})
		return
	}

	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      "system",
			EventType:  "discovery.aws.scan_completed",
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   accountID,
			Action:     "scan_completed",
			Payload: map[string]any{
				"account_id":           accountID,
				"scan_id":              result.ScanID,
				"compute_count":        len(result.Compute),
				"function_count":       len(result.Functions),
				"instrumented_count":   result.InstrumentedCount,
				"uninstrumented_count": result.UninstrumentedCount,
				"partial":              result.Partial,
				"recorded_at":          time.Now().UTC(),
			},
		})
	}

	c.JSON(http.StatusOK, marshalScanResult(result))
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

	result, err := h.aiProposer.ProposeFromDiscoveryScan(c.Request.Context(), aiCtx)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("aws generate recommendations: proposer call failed",
				zap.Error(err), zap.String("account_id", accountID), zap.String("scan_id", aiCtx.ScanID))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "ProposerCallFailed",
			Message: "Squadron's AI proposer failed: " + err.Error(),
		}})
		return
	}

	if result.Declined {
		// Surface the model's reason; no audit event for
		// recommendations_generated (nothing was generated). An empty
		// Recommendations array — never null — so the UI's branch on
		// .length stays simple.
		c.JSON(http.StatusOK, awsGenerateRecommendationsResponse{
			Declined:        true,
			Reason:          result.Reason,
			Recommendations: []recommendations.Recommendation{},
		})
		return
	}

	// Walk the plan-kind result into one Recommendation per step. Each
	// step's Terraform lands in the typed IaC field (the v0.85 Stream
	// 2B addition); the Action payload carries the step JSON for the
	// UI's preview flow.
	now := time.Now().UTC()
	recs := make([]recommendations.Recommendation, 0, len(result.Plan.Steps))
	for i, step := range result.Plan.Steps {
		stepJSON, err := json.Marshal(step)
		if err != nil {
			// Marshal of a Go struct we just produced should never
			// fail. Surface as 500 so the operator sees a clean
			// error rather than a half-filled recommendations list.
			if h.logger != nil {
				h.logger.Error("aws generate recommendations: plan step marshal failed",
					zap.Error(err), zap.Int("step_index", i))
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
				Code:    "PlanStepMarshalFailed",
				Message: "Squadron could not encode the plan step. The error has been logged.",
			}})
			return
		}
		title := step.Name
		if title == "" {
			title = "Discovery recommendation"
		}
		detail := result.Reasoning
		if detail == "" {
			detail = "AI-emitted instrumentation plan step. Run the Terraform through your IaC pipeline."
		}
		rec := recommendations.Recommendation{
			ID:              "discovery-" + req.ScanResult.ScanID + "-" + strconv.Itoa(i),
			Category:        recommendations.CategoryEmptySignal, // closest existing semantic match: "resource emits no telemetry"
			Severity:        recommendations.SeverityWarn,
			Title:           title,
			Detail:          detail,
			EstSavingsBytes: 0,
			GeneratedAt:     now,
			Source: &recommendations.RecommendationSource{
				Kind:  recommendations.SourceDiscoveryScan,
				RefID: req.ScanResult.ScanID,
			},
			Action: &recommendations.RecommendationAction{
				Kind:    recommendations.ActionPlan,
				Payload: stepJSON,
			},
			IaC: &recommendations.IaCSnippet{
				Format: recommendations.IaCTerraform,
				Source: step.InlineConfigSnippet,
			},
		}
		recs = append(recs, rec)
	}

	// Audit event. Payload deliberately omits the Terraform content —
	// audit rows shouldn't grow with snippet size. step_count +
	// scan_id + token metering are what an auditor needs to
	// reconstruct "what was generated and how much did it cost".
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
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
	}

	c.JSON(http.StatusOK, awsGenerateRecommendationsResponse{
		Declined:        false,
		Reasoning:       result.Reasoning,
		Recommendations: recs,
	})
}
