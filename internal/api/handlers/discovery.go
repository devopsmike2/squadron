// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/services"
)

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
	auditService      services.AuditService
	logger            *zap.Logger
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
func (h *DiscoveryHandlers) WithCredstoreKey(key *credstore.Key) *DiscoveryHandlers {
	h.awsCredMarshaller = func(creds credstore.AWSCredentials) ([]byte, []byte, error) {
		return credstore.MarshalAWSCredentials(creds, key)
	}
	return h
}

// WithCredMarshaller overrides the AWSCredMarshaller. Tests use this
// to inject a pass-through that records the cleartext creds without
// invoking the AEAD; production callers use WithCredstoreKey.
func (h *DiscoveryHandlers) WithCredMarshaller(m AWSCredMarshaller) *DiscoveryHandlers {
	h.awsCredMarshaller = m
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
