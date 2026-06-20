// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
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

	// Pull the smithy.APIError shape if present — every typed AWS
	// SDK v2 error implements it. Fall through to the default
	// message when the underlying error is not an APIError (e.g. a
	// DNS failure or a context cancellation).
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied":
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
