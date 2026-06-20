// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package wizard

import (
	"regexp"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// TestAWSWizardHasFiveSteps locks the slice-1 step count. A change to
// this assertion is a signal to revisit the design doc's "Connector
// workflow design > Architecture" section — adding or removing a step
// without updating the doc is a process violation.
func TestAWSWizardHasFiveSteps(t *testing.T) {
	w := AWSWizard()
	if got, want := len(w.Steps), 5; got != want {
		t.Fatalf("AWSWizard step count = %d, want %d", got, want)
	}
	if w.Provider != credstore.ProviderAWS {
		t.Errorf("AWSWizard.Provider = %q, want %q", w.Provider, credstore.ProviderAWS)
	}
	if strings.TrimSpace(w.Title) == "" {
		t.Errorf("AWSWizard.Title should not be empty")
	}
}

// TestAWSWizardStepsHaveRequiredFields enforces the
// foolproof-or-release-blocked invariant: every step must carry a
// non-empty ID, Title, Description, DocLink, and RecoveryHint. A step
// missing any of these would break the "why this step?" panel or the
// jump-back UX, both of which the design doc treats as load-bearing.
func TestAWSWizardStepsHaveRequiredFields(t *testing.T) {
	w := AWSWizard()
	for i, step := range w.Steps {
		t.Run(step.ID, func(t *testing.T) {
			if strings.TrimSpace(step.ID) == "" {
				t.Errorf("step %d: ID is empty", i)
			}
			if strings.TrimSpace(step.Title) == "" {
				t.Errorf("step %s: Title is empty", step.ID)
			}
			if strings.TrimSpace(step.Description) == "" {
				t.Errorf("step %s: Description is empty", step.ID)
			}
			if strings.TrimSpace(step.DocLink) == "" {
				t.Errorf("step %s: DocLink is empty", step.ID)
			}
			if strings.TrimSpace(step.RecoveryHint) == "" {
				t.Errorf("step %s: RecoveryHint is empty", step.ID)
			}
			// Regex-validated steps must carry a non-empty Pattern
			// and Message — otherwise the inline check is silent.
			if step.Validation.Kind == ValidationRegex {
				if step.Validation.Pattern == "" {
					t.Errorf("step %s: regex validation with empty pattern", step.ID)
				}
				if step.Validation.Message == "" {
					t.Errorf("step %s: regex validation with empty message", step.ID)
				}
			}
		})
	}
}

// TestAccountIDValidation drives the regex on the account-id step.
// The wizard's inline rule MUST match exactly the values Squadron's
// API accepts; a mismatch would mean an operator can pass the wizard
// check and then fail the server validation, which violates the
// "errors appear inline, not after submit" principle.
func TestAccountIDValidation(t *testing.T) {
	step := findStep(t, "account-id")
	if step.Validation.Kind != ValidationRegex {
		t.Fatalf("account-id step validation kind = %q, want regex", step.Validation.Kind)
	}
	re, err := regexp.Compile(step.Validation.Pattern)
	if err != nil {
		t.Fatalf("account-id regex did not compile: %v", err)
	}

	cases := []struct {
		input string
		want  bool
	}{
		{"123456789012", true},     // canonical 12-digit account
		{"000000000000", true},     // all zeros — AWS allows; format-wise fine
		{"abc", false},             // non-numeric
		{"12345", false},           // too short
		{"123456789012345", false}, // too long
		{"12345678901a", false},    // mostly digits but one letter
		{"", false},                // empty
		{" 123456789012 ", false},  // whitespace — operator must trim
	}
	for _, c := range cases {
		if got := re.MatchString(c.input); got != c.want {
			t.Errorf("regex.MatchString(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// TestRoleARNValidation drives the regex on the role-arn step. Same
// rationale as TestAccountIDValidation: the inline rule must match the
// server side.
func TestRoleARNValidation(t *testing.T) {
	step := findStep(t, "role-arn")
	if step.Validation.Kind != ValidationRegex {
		t.Fatalf("role-arn step validation kind = %q, want regex", step.Validation.Kind)
	}
	re, err := regexp.Compile(step.Validation.Pattern)
	if err != nil {
		t.Fatalf("role-arn regex did not compile: %v", err)
	}

	cases := []struct {
		input string
		want  bool
	}{
		{"arn:aws:iam::123456789012:role/SquadronDiscovery", true},
		{"arn:aws:iam::123456789012:role/path-with-dashes_and+chars.@equals=", true},
		{"arn:aws:iam::000000000000:role/x", true},
		// Bad shapes:
		{"arn:aws:iam::123:role/x", false},             // account ID too short
		{"arn:aws:iam::role/SquadronDiscovery", false}, // missing account ID
		{"arn:aws:s3:::my-bucket", false},              // not an IAM role
		{"SquadronDiscovery", false},                   // just a role name
		{"", false},
		{"arn:aws:iam::123456789012:role/", false}, // empty role name
	}
	for _, c := range cases {
		if got := re.MatchString(c.input); got != c.want {
			t.Errorf("regex.MatchString(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// TestAllStepIDsUnique enforces the ID uniqueness contract the
// HumanizedError.SuggestedStep pointer depends on. Duplicate IDs
// would make the jump-back UX ambiguous.
func TestAllStepIDsUnique(t *testing.T) {
	w := AWSWizard()
	seen := make(map[string]int, len(w.Steps))
	for i, step := range w.Steps {
		if prev, ok := seen[step.ID]; ok {
			t.Errorf("duplicate step ID %q at indices %d and %d", step.ID, prev, i)
		}
		seen[step.ID] = i
	}
}

// findStep returns the wizard step with the given ID, failing the test
// if no match is found. Used by the per-step regex tests so each test
// pinpoints the exact step rather than indexing by position (which
// would break silently if the step order changes).
func findStep(t *testing.T, id string) WizardStep {
	t.Helper()
	w := AWSWizard()
	for _, step := range w.Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("no step with ID %q in AWSWizard", id)
	return WizardStep{}
}
