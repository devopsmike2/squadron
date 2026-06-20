// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/smithy-go"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Wizard step IDs the humanizer hands back. They mirror the slice-1
// AWS ConnectorWizard step IDs documented in the design doc's
// "Connector workflow design" section. Keeping them as constants here
// (rather than re-using literals scattered through the package) means
// a step rename only touches one place.
const (
	stepTrustPolicy = "trust-policy"
	stepRoleARN     = "role-arn"
	stepValidate    = "validate"
)

// ErrCodeNoCredentials is the humanized error code the wizard renders
// when Squadron itself has no AWS credentials configured (the base
// identity that calls sts:AssumeRole). It is a Squadron-host config
// problem, not a customer-role problem — the SuggestedStep points the
// operator at the role-arn step only for navigation context; the fix
// is to set AWS_REGION + AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY in
// the Squadron process environment or to run Squadron on an
// EC2/ECS/EKS instance with an IAM role attached.
const ErrCodeNoCredentials = "no_credentials"

// isNoCredentialsError reports whether err is the result of credential
// discovery failing — either the credentialDiscoveryTimeout fired (the
// IMDSv2 probe couldn't reach 169.254.169.254) or the SDK explicitly
// said no credentials were available. Matched substrings:
//   - "failed to retrieve credentials" — SDK v2's wrapping of a
//     credential-chain dry run.
//   - "no EC2 IMDS role found" — the IMDS-specific signal when the
//     instance has no role attached.
//   - "NoCredentialProviders" — older SDK shape, defensive.
//
// The DeadlineExceeded branch is intentionally broad: in production
// the only context that gets a sub-second budget on this code path is
// the credential-discovery context from client.go.
func isNoCredentialsError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "failed to retrieve credentials"):
		return true
	case strings.Contains(msg, "no EC2 IMDS role found"):
		return true
	case strings.Contains(msg, "NoCredentialProviders"):
		return true
	}
	return false
}

// noCredentialsHumanizedError builds the wizard-facing payload for the
// "Squadron has no AWS credentials" failure mode. Extracted so both
// HumanizeError and the validate handler can produce the same shape
// without duplicating the message text.
func noCredentialsHumanizedError() *scanner.HumanizedError {
	return &scanner.HumanizedError{
		Code:          ErrCodeNoCredentials,
		Message:       "Squadron has no AWS credentials configured. Set AWS_REGION + AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY in Squadron's environment, or run Squadron on an EC2/ECS/EKS instance with an IAM role attached.",
		SuggestedStep: stepRoleARN,
		DocLink:       "https://docs.aws.amazon.com/sdkref/latest/guide/standardized-credentials.html",
	}
}

// HumanizeError converts a raw AWS SDK error into a wizard-friendly
// HumanizedError. The wizard renders Message verbatim; SuggestedStep
// drives the deep-link back to the step the operator needs to fix.
//
// Returns nil when err is nil — call sites can blindly pass through
// without nil-checking, and an absent HumanizedError in the
// ValidationResult unambiguously means "no problem here".
//
// Unknown error codes fall through to a default message that names
// the role-arn step (the most likely culprit for a generic failure)
// and embeds the raw error string so support agents have something to
// grep against.
func HumanizeError(err error) *scanner.HumanizedError {
	if err == nil {
		return nil
	}

	// Check the "no credentials configured" branch BEFORE the
	// smithy.APIError check. The SDK v2 wraps a credential-chain
	// dry-run failure as a plain error (not an APIError), and the
	// v0.85.0 post-ship E2E sweep showed that letting it fall through
	// to the default fallback surfaces a confusing "AWS returned an
	// error: failed to retrieve credentials" message that doesn't
	// name the recoverable action (set env vars OR run on EC2). The
	// dedicated branch makes the fix discoverable.
	if isNoCredentialsError(err) {
		return noCredentialsHumanizedError()
	}

	// Pull the smithy.APIError shape if present — every typed AWS
	// SDK v2 error implements it. Fall through to the default
	// message when the underlying error is not an APIError (e.g. a
	// DNS failure or a context cancellation).
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied":
			// AccessDenied surfaces uniformly across every service the
			// scanner walks — ec2:DescribeInstances, lambda:ListFunctions,
			// rds:DescribeDBInstances (slice 2). The recoverable action
			// is always the same: re-paste the trust policy from Step 2,
			// which is what the wizard's deep-link target covers. The
			// service-specific Action name is preserved in the raw error
			// message the model field carries, but the humanized step
			// pointer stays generic so an operator who's failing on the
			// third service doesn't see a different wizard navigation
			// hint than they'd see on the first.
			return &scanner.HumanizedError{
				Code:          "AccessDenied",
				Message:       "The role's trust policy doesn't authorize Squadron's principal. Did you paste the trust policy from Step 2?",
				SuggestedStep: stepTrustPolicy,
			}
		case "MalformedPolicyDocument":
			return &scanner.HumanizedError{
				Code:          "MalformedPolicyDocument",
				Message:       "The role's trust policy has a syntax error. Re-copy from Step 2 — don't edit the JSON.",
				SuggestedStep: stepTrustPolicy,
			}
		case "InvalidClientTokenId":
			return &scanner.HumanizedError{
				Code:          "InvalidClientTokenId",
				Message:       "The role ARN doesn't exist or your Squadron deployment can't reach AWS. Verify the ARN matches Step 3.",
				SuggestedStep: stepRoleARN,
			}
		case "Throttling", "ThrottlingException":
			return &scanner.HumanizedError{
				Code:          apiErr.ErrorCode(),
				Message:       "AWS is rate-limiting the scan. Wait 30 seconds and retry.",
				SuggestedStep: stepValidate,
			}
		}
		// Known APIError but unmapped code: include both code and
		// message in the default fallback so the operator's
		// support thread has something concrete.
		return &scanner.HumanizedError{
			Code:          apiErr.ErrorCode(),
			Message:       fmt.Sprintf("AWS returned an error: %s. Check the role configuration in AWS IAM.", strings.TrimSpace(apiErr.ErrorMessage())),
			SuggestedStep: stepRoleARN,
		}
	}

	// Non-API error — the underlying transport or context layer
	// failed. The wizard still needs a message; include the raw
	// error string trimmed of whitespace for readability.
	return &scanner.HumanizedError{
		Code:          "",
		Message:       fmt.Sprintf("AWS returned an error: %s. Check the role configuration in AWS IAM.", strings.TrimSpace(err.Error())),
		SuggestedStep: stepRoleARN,
	}
}
