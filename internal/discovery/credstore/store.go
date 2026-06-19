// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"time"
)

// AWSConnection is the trust-policy metadata for one connected AWS
// account. ExternalID is the sensitive shared secret used by the
// customer's trust policy to defeat the confused-deputy problem; it is
// encrypted at rest and never written to logs or audit payloads.
//
// AccountID is the primary identifier — one row per account. RoleARN
// is the IAM role Squadron assumes into. Region is the operator-chosen
// primary region for scans (slice 1 is single-region; later slices
// expand to multi-region scanning from this anchor).
type AWSConnection struct {
	AccountID   string    `json:"account_id"`
	RoleARN     string    `json:"role_arn"`
	ExternalID  string    `json:"-"` // sensitive — never serialized to JSON or logged
	DisplayName string    `json:"display_name"`
	Region      string    `json:"region"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store is the credential substrate interface. Implementations encrypt
// ExternalID at rest with AES-256-GCM and emit a discovery.role_assumed
// audit event on every read.
//
// All methods are safe for concurrent use; the SQLite-backed default
// serializes writes through the underlying database connection pool.
type Store interface {
	// StoreAWSConnection inserts or updates a connection record. The
	// ExternalID is encrypted before persistence; the stored row never
	// contains plaintext. CreatedAt is preserved on update; UpdatedAt
	// is always stamped to now.
	StoreAWSConnection(ctx context.Context, conn AWSConnection) error

	// GetAWSConnection returns the connection record for the given AWS
	// account ID with the ExternalID decrypted in memory. Returns
	// (nil, nil) if no row matches. Emits a discovery.role_assumed
	// audit event on every successful read.
	GetAWSConnection(ctx context.Context, accountID string) (*AWSConnection, error)

	// ListAWSConnections returns every connection record. Each row's
	// ExternalID is decrypted in memory before return. Emits one
	// discovery.role_assumed audit event covering the list operation.
	ListAWSConnections(ctx context.Context) ([]*AWSConnection, error)

	// DeleteAWSConnection removes the row for the given AWS account ID.
	// Idempotent: deleting a non-existent account is not an error.
	DeleteAWSConnection(ctx context.Context, accountID string) error

	// Close releases the underlying database handle. Subsequent calls
	// to other methods return an error.
	Close() error
}
