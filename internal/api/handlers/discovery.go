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
	awsorch "github.com/devopsmike2/squadron/internal/discovery/aws"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/iac"
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
	ECSClusters         []awsECSClusterRow `json:"ecs_clusters"`
	InstrumentedCount   int                `json:"instrumented_count"`
	UninstrumentedCount int                `json:"uninstrumented_count"`
	Partial             bool               `json:"partial"`
	PartialReason       string             `json:"partial_reason,omitempty"`
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

// awsDatabaseInstanceRow is the snake_case wire shape for one RDS row.
// Mirrors scanner.DatabaseInstanceSnapshot — the two observability
// lever flags surface as separate booleans so the Inventory tab can
// render them as independent badge columns, matching the proposer
// prompt's "treat PI + EM as independent levers" framing.
type awsDatabaseInstanceRow struct {
	ResourceID                 string            `json:"resource_id"`
	Engine                     string            `json:"engine"`
	EngineVersion              string            `json:"engine_version"`
	InstanceClass              string            `json:"instance_class"`
	PerformanceInsightsEnabled bool              `json:"performance_insights_enabled"`
	EnhancedMonitoringEnabled  bool              `json:"enhanced_monitoring_enabled"`
	Region                     string            `json:"region"`
	Tags                       map[string]string `json:"tags"`
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
		Databases:           make([]awsDatabaseInstanceRow, 0, len(r.Databases)),
		ObjectStores:        make([]awsObjectStoreRow, 0, len(r.ObjectStores)),
		LoadBalancers:       make([]awsLoadBalancerRow, 0, len(r.LoadBalancers)),
		Clusters:            make([]awsClusterRow, 0, len(r.Clusters)),
		DynamoDBTables:      make([]awsDynamoDBTableRow, 0, len(r.DynamoDBTables)),
		ECSClusters:         make([]awsECSClusterRow, 0, len(r.ECSClusters)),
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
	for _, db := range r.Databases {
		out.Databases = append(out.Databases, awsDatabaseInstanceRow{
			ResourceID:                 db.ResourceID,
			Engine:                     db.Engine,
			EngineVersion:              db.EngineVersion,
			InstanceClass:              db.InstanceClass,
			PerformanceInsightsEnabled: db.PerformanceInsightsEnabled,
			EnhancedMonitoringEnabled:  db.EnhancedMonitoringEnabled,
			Region:                     db.Region,
			Tags:                       db.Tags,
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

	// runAWSScan emits scan_started + scan_completed audit events and
	// drives the scanner. The single-account endpoint passes an empty
	// scanAllID so the per-account scan_completed event omits the
	// scan_all_id field (v0.89.7a trace linkage is only present when
	// the orchestrator drives the per-account scan). The third return
	// value is the HTTP status the handler should emit on failure;
	// the orchestrator path ignores it.
	result, herr, status := h.runAWSScan(c.Request.Context(), accountID, req.Regions, "" /* scanAllID */)
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
func (h *DiscoveryHandlers) runAWSScan(ctx context.Context, accountID string, requestedRegions []string, scanAllID string) (*scanner.Result, *scanner.HumanizedError, int) {
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
	ScanAllID           string                       `json:"scan_all_id"`
	TotalAccounts       int                          `json:"total_accounts"`
	SucceededAccounts   []awsScanAllAccountRow       `json:"succeeded_accounts"`
	FailedAccounts      []awsScanAllFailureRow       `json:"failed_accounts"`
	TotalResources      int                          `json:"total_resources"`
	TotalInstrumented   int                          `json:"total_instrumented"`
	TotalUninstrumented int                          `json:"total_uninstrumented"`
	Partial             bool                         `json:"partial"`
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
		result, herr, _ := h.runAWSScan(ctx, conn.AccountID, regs, scanAllID)
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
		aiCtx.Databases = append(aiCtx.Databases, ai.DatabaseResourceCandidate{
			ResourceID:                 db.ResourceID,
			Engine:                     db.Engine,
			EngineVersion:              db.EngineVersion,
			InstanceClass:              db.InstanceClass,
			PerformanceInsightsEnabled: db.PerformanceInsightsEnabled,
			EnhancedMonitoringEnabled:  db.EnhancedMonitoringEnabled,
			Region:                     db.Region,
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
		// v0.88.4: per-step Detail was previously set to result.Reasoning,
		// which duplicated the overall proposer-reasoning text into every
		// step card. The Recommendations tab already renders the overall
		// reasoning in a single panel at the top of the page; surfacing
		// the same string on every step was operator-visible noise that
		// pushed the actual Terraform snippet down the fold. v0.88.4
		// leaves Detail as a short generic per-step descriptor; the
		// proposer's overall narrative is read once from the top-level
		// Reasoning field.
		detail := "AI-emitted instrumentation plan step. Run the Terraform through your IaC pipeline."
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
			// v0.89.3 #603 Stream 19 Phase 4: classify the step into one
			// of the slice-1 placement-map kinds so the Recommendations
			// tab's Open-PR button can look up the right placement row.
			// Empty when the step's snippet doesn't match any known
			// Terraform resource shape — the UI falls back to Copy-only
			// in that case.
			ResourceKind: classifyResourceKind(step.Name, step.InlineConfigSnippet),
			// v0.89.4 #611 Stream 19 Phase 4 follow-on: thread the
			// proposer-emitted per-step affected_resources list through
			// so the Open-PR backend's PR title's "for <N> resources"
			// count and the body's "Affected resources" bullet list
			// reflect the actual resource population. Empty slice
			// when the model didn't emit it — the backend's title
			// falls back to "for 0 resources" rather than erroring.
			AffectedResources: append([]string(nil), step.AffectedResources...),
		}
		// v0.89.11 #626 Stream 27 (slice 1.5): stamp the canonical
		// disposition keyed off the classifier result. The proposer
		// may emit step.Disposition but the handler-side
		// classification is the authoritative one (structural fact,
		// not a model judgment). Empty ResourceKind → empty
		// Disposition so non-Open-PR-eligible recommendations don't
		// carry a confusing badge.
		if rec.ResourceKind != "" {
			rec.Disposition = iac.DispositionFor(rec.ResourceKind)
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
