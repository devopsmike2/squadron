// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
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
// ships AWS validate; future slices add GCP and Azure equivalents
// behind the same handler shape.
//
// credStore is reserved for the (future) Save endpoint that lands in
// slice 1's UI work. The validate handler does NOT write to the
// store; the field is here so the handler type doesn't have to grow
// a second constructor when Save lands.
type DiscoveryHandlers struct {
	credStore       credstore.Store
	awsValidatorFor AWSValidatorFactory
	logger          *zap.Logger
}

// NewDiscoveryHandlers builds a DiscoveryHandlers wired with the
// default production AWS validator factory. credStore may be nil at
// construction — slice 1's validate endpoint doesn't read or write
// it; future Save endpoints will. logger must be non-nil.
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
