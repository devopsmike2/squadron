// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
)

// AWSCredentials is the slice-1 provider-typed authentication material
// for a CloudConnection whose Provider == ProviderAWS. The shape mirrors
// the IAM trust-policy contract documented in
// docs/universal-discovery-design.md "Security architecture > IAM trust
// policy":
//
//   - RoleARN identifies the customer IAM role Squadron assumes into.
//     Not a secret per se, but kept inside the encrypted blob so the
//     substrate has a uniform "everything provider-specific is
//     encrypted" posture rather than mixing plaintext and ciphertext
//     fields per provider.
//   - ExternalID is the per-deployment shared secret that defeats the
//     confused-deputy problem. Sensitive — never logged, never put in
//     an audit payload, never serialized in plaintext outside of the
//     in-memory struct.
//
// This is the ONLY AWS-specific code in the credstore package. The
// substrate is provider-agnostic by design; each provider's scanner
// package owns its own Marshal/Unmarshal helpers built on the
// SecretsBackend. AWS lives here because slice 1 is AWS-only and a
// new package for one struct would be premature.
type AWSCredentials struct {
	RoleARN    string `json:"role_arn"`
	ExternalID string `json:"external_id"`
}

// MarshalAWSCredentials serializes creds to JSON and encrypts the
// payload with the supplied Key. Returns (ciphertext, nonce, error)
// ready to assign to CloudConnection.Credentials and
// CloudConnection.CredentialsNonce.
//
// The Key is the SecretsBackend-internal primitive — callers using the
// SQLiteSecretsBackend pass its embedded Key. This signature
// intentionally takes a *Key rather than a SecretsBackend so the AWS
// helpers stay focused on shape-marshaling; nothing AWS-specific
// depends on the backend choice.
func MarshalAWSCredentials(creds AWSCredentials, key *Key) ([]byte, []byte, error) {
	if key == nil {
		return nil, nil, errors.New("credstore: MarshalAWSCredentials: key is required")
	}
	if creds.RoleARN == "" {
		return nil, nil, errors.New("credstore: MarshalAWSCredentials: RoleARN is required")
	}
	if creds.ExternalID == "" {
		// Empty ExternalID would bypass the confused-deputy defense.
		// The trust-policy template generated for operators always
		// includes one, so an empty value here is a programming error.
		return nil, nil, errors.New("credstore: MarshalAWSCredentials: ExternalID is required (trust policy is unsafe without it)")
	}
	plaintext, err := json.Marshal(creds)
	if err != nil {
		return nil, nil, fmt.Errorf("credstore: marshal AWSCredentials: %w", err)
	}
	ciphertext, nonce, err := key.Seal(plaintext)
	if err != nil {
		return nil, nil, fmt.Errorf("credstore: encrypt AWSCredentials: %w", err)
	}
	return ciphertext, nonce, nil
}

// UnmarshalAWSCredentials decrypts ciphertext with the supplied Key
// and JSON-decodes the result back into an AWSCredentials struct.
// Safe to call only when the originating CloudConnection.Provider was
// ProviderAWS — calling against another provider's blob will succeed
// only if the JSON happens to parse, which is not a security
// guarantee. Callers branch on Provider before reaching here.
func UnmarshalAWSCredentials(ciphertext, nonce []byte, key *Key) (*AWSCredentials, error) {
	if key == nil {
		return nil, errors.New("credstore: UnmarshalAWSCredentials: key is required")
	}
	plaintext, err := key.Open(ciphertext, nonce)
	if err != nil {
		return nil, err
	}
	var creds AWSCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return nil, fmt.Errorf("credstore: unmarshal AWSCredentials: %w", err)
	}
	return &creds, nil
}
