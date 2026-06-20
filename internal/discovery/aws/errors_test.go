// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	smithy "github.com/aws/smithy-go"
)

// apiErr is a tiny smithy.APIError implementation the tests use to
// drive the humanizer. The real AWS SDK types implement the same
// interface but live behind dozens of types; spelling out a tiny
// stand-in keeps the test setup readable.
type apiErr struct {
	code string
	msg  string
}

func (e *apiErr) Error() string                 { return fmt.Sprintf("%s: %s", e.code, e.msg) }
func (e *apiErr) ErrorCode() string             { return e.code }
func (e *apiErr) ErrorMessage() string          { return e.msg }
func (e *apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestHumanizeError_NilSafety(t *testing.T) {
	if got := HumanizeError(nil); got != nil {
		t.Fatalf("HumanizeError(nil) = %+v, want nil", got)
	}
}

func TestHumanizeError_KnownCodes(t *testing.T) {
	cases := []struct {
		name          string
		code          string
		wantStep      string
		wantContains  string // substring expected in Message
		wantCodeOnOut string // expected Code on the result
	}{
		{
			name:          "AccessDenied points operator at trust policy step",
			code:          "AccessDenied",
			wantStep:      "trust-policy",
			wantContains:  "trust policy",
			wantCodeOnOut: "AccessDenied",
		},
		{
			name:          "MalformedPolicyDocument also points at trust policy step",
			code:          "MalformedPolicyDocument",
			wantStep:      "trust-policy",
			wantContains:  "Re-copy from Step 2",
			wantCodeOnOut: "MalformedPolicyDocument",
		},
		{
			name:          "InvalidClientTokenId points at role-arn step",
			code:          "InvalidClientTokenId",
			wantStep:      "role-arn",
			wantContains:  "role ARN",
			wantCodeOnOut: "InvalidClientTokenId",
		},
		{
			name:          "Throttling points at validate retry step",
			code:          "Throttling",
			wantStep:      "validate",
			wantContains:  "rate-limit",
			wantCodeOnOut: "Throttling",
		},
		{
			name:          "ThrottlingException also points at validate",
			code:          "ThrottlingException",
			wantStep:      "validate",
			wantContains:  "rate-limit",
			wantCodeOnOut: "ThrottlingException",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &apiErr{code: tc.code, msg: "raw aws message"}
			got := HumanizeError(err)
			if got == nil {
				t.Fatalf("HumanizeError returned nil")
			}
			if got.SuggestedStep != tc.wantStep {
				t.Errorf("SuggestedStep = %q, want %q", got.SuggestedStep, tc.wantStep)
			}
			if !strings.Contains(got.Message, tc.wantContains) {
				t.Errorf("Message %q missing substring %q", got.Message, tc.wantContains)
			}
			if got.Code != tc.wantCodeOnOut {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCodeOnOut)
			}
		})
	}
}

func TestHumanizeError_DefaultFallback_APIErrorUnknownCode(t *testing.T) {
	err := &apiErr{code: "SomethingNovel", msg: "the role got cosmic-rayed"}
	got := HumanizeError(err)
	if got == nil {
		t.Fatalf("HumanizeError returned nil")
	}
	if got.SuggestedStep != "role-arn" {
		t.Errorf("SuggestedStep = %q, want role-arn (the safe default)", got.SuggestedStep)
	}
	if !strings.Contains(got.Message, "the role got cosmic-rayed") {
		t.Errorf("Message %q should embed the raw AWS message", got.Message)
	}
	if got.Code != "SomethingNovel" {
		t.Errorf("Code = %q, want SomethingNovel (the raw code)", got.Code)
	}
}

func TestHumanizeError_DefaultFallback_NonAPIError(t *testing.T) {
	err := errors.New("dial tcp: i/o timeout")
	got := HumanizeError(err)
	if got == nil {
		t.Fatalf("HumanizeError returned nil")
	}
	if got.SuggestedStep != "role-arn" {
		t.Errorf("SuggestedStep = %q, want role-arn", got.SuggestedStep)
	}
	if !strings.Contains(got.Message, "dial tcp") {
		t.Errorf("Message %q should embed the raw error string", got.Message)
	}
	if got.Code != "" {
		t.Errorf("Code = %q, want empty for non-APIError input", got.Code)
	}
}

// TestHumanizeError_NoCredentials_DeadlineExceeded verifies that a bare
// context.DeadlineExceeded — which is what the 5s
// credentialDiscoveryTimeout in client.go produces when IMDSv2 is
// unreachable — maps to the NoCredentials humanized error. The wizard
// must surface the recoverable action (set env vars or run on EC2),
// NOT a generic "AWS returned an error: context deadline exceeded".
func TestHumanizeError_NoCredentials_DeadlineExceeded(t *testing.T) {
	got := HumanizeError(context.DeadlineExceeded)
	if got == nil {
		t.Fatalf("HumanizeError(context.DeadlineExceeded) returned nil")
	}
	if got.Code != ErrCodeNoCredentials {
		t.Errorf("Code = %q, want %q", got.Code, ErrCodeNoCredentials)
	}
	if !strings.Contains(got.Message, "AWS_ACCESS_KEY_ID") {
		t.Errorf("Message %q should name the env vars the operator must set", got.Message)
	}
	if !strings.Contains(got.Message, "EC2/ECS/EKS") {
		t.Errorf("Message %q should mention the EC2/ECS/EKS instance-role alternative", got.Message)
	}
	if got.SuggestedStep != "role-arn" {
		t.Errorf("SuggestedStep = %q, want role-arn (the wizard navigation anchor)", got.SuggestedStep)
	}
	if got.DocLink == "" {
		t.Errorf("DocLink should be populated so operators can read the standardized-credentials reference")
	}
}

// TestHumanizeError_NoCredentials_DeadlineExceeded_Wrapped covers the
// production shape — newSDKClientFactory wraps the SDK error via
// fmt.Errorf("aws: load default config: %w", err) before it reaches
// HumanizeError. errors.Is must walk the wrap chain.
func TestHumanizeError_NoCredentials_DeadlineExceeded_Wrapped(t *testing.T) {
	wrapped := fmt.Errorf("aws: load default config: %w", context.DeadlineExceeded)
	got := HumanizeError(wrapped)
	if got == nil {
		t.Fatalf("HumanizeError(wrapped DeadlineExceeded) returned nil")
	}
	if got.Code != ErrCodeNoCredentials {
		t.Errorf("Code = %q, want %q (errors.Is must unwrap)", got.Code, ErrCodeNoCredentials)
	}
}

// TestHumanizeError_NoCredentials_NoIMDSRole covers the SDK's
// "no EC2 IMDS role found" signal — what an EC2 instance with no role
// attached returns from the credential chain. Same humanized error
// applies: Squadron's host needs credentials.
func TestHumanizeError_NoCredentials_NoIMDSRole(t *testing.T) {
	err := errors.New("operation error: no EC2 IMDS role found")
	got := HumanizeError(err)
	if got == nil {
		t.Fatalf("HumanizeError returned nil")
	}
	if got.Code != ErrCodeNoCredentials {
		t.Errorf("Code = %q, want %q", got.Code, ErrCodeNoCredentials)
	}
	if !strings.Contains(got.Message, "AWS_ACCESS_KEY_ID") {
		t.Errorf("Message %q should name the env vars to set", got.Message)
	}
}

// TestHumanizeError_NoCredentials_FailedToRetrieve covers the SDK v2
// wrapping shape — when the credential chain dry-runs and produces
// "failed to retrieve credentials: ..." with the underlying provider
// errors attached. The substring match keeps the humanizer robust to
// future SDK message tweaks.
func TestHumanizeError_NoCredentials_FailedToRetrieve(t *testing.T) {
	err := errors.New("aws: load default config: failed to retrieve credentials: no providers in chain")
	got := HumanizeError(err)
	if got == nil {
		t.Fatalf("HumanizeError returned nil")
	}
	if got.Code != ErrCodeNoCredentials {
		t.Errorf("Code = %q, want %q", got.Code, ErrCodeNoCredentials)
	}
}

// TestHumanizeError_NoCredentials_LegacyNoCredentialProviders covers
// the older SDK shape some transitive dependencies still surface.
// Defensive match — keeps the wizard friendly even if a future
// downstream update reintroduces the legacy error string.
func TestHumanizeError_NoCredentials_LegacyNoCredentialProviders(t *testing.T) {
	err := errors.New("NoCredentialProviders: no valid providers in chain")
	got := HumanizeError(err)
	if got == nil {
		t.Fatalf("HumanizeError returned nil")
	}
	if got.Code != ErrCodeNoCredentials {
		t.Errorf("Code = %q, want %q", got.Code, ErrCodeNoCredentials)
	}
}
