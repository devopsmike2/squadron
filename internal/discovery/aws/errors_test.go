// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
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
