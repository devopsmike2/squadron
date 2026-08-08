// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// ConnectorCredentials is the plaintext secret shape for one connector
// instance: the material needed to authenticate against a telemetry
// backend. Per ADR 0034, credentials never live in the plain Config or
// in logs — they are sealed at rest and only ever held in memory inside
// this struct.
//
// Sealing REUSES the discovery credstore's AES-256-GCM primitive
// (credstore.Key, keyed by SQUADRON_SECRETS_KEY) rather than inventing a
// second scheme, exactly as the AWS/GCP/Azure/OCI discovery connectors
// and the IaC PAT store already do. The blob layout ([12-byte nonce ||
// ciphertext]) matches iacconnstore's sealed-blob convention so operators
// have one sealing story across every connection substrate.
type ConnectorCredentials struct {
	// Token is the primary bearer secret: an API key, a Splunk HEC
	// token, a Datadog API key, or an OAuth/bearer token. Empty when the
	// backend uses basic auth instead.
	Token string `json:"token,omitempty"`

	// Username and Password authenticate against backends that use HTTP
	// basic auth (e.g. Loki behind a basic-auth gateway). Empty when the
	// backend uses a bearer Token instead.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`

	// Headers carries extra secret headers some backends require beyond
	// a single token (e.g. Datadog's paired API + application keys).
	// Kept inside the sealed blob so nothing sensitive lands in the
	// plain Config.
	Headers map[string]string `json:"headers,omitempty"`
}

// IsZero reports whether no credential material is set. A connector for
// an unauthenticated backend (a local dev Loki) may legitimately have
// zero credentials.
func (c ConnectorCredentials) IsZero() bool {
	return c.Token == "" && c.Username == "" && c.Password == "" && len(c.Headers) == 0
}

// nonceLen is the AES-GCM nonce length credstore.Key uses (12 bytes).
// Duplicated as a local constant, mirroring iacconnstore.github_pat, so
// a future change to credstore's nonce size is caught by the round-trip
// tests at compile/test time rather than silently mis-packing the blob.
const nonceLen = 12

// errCredMarshalFailed is the opaque error returned when sealing fails.
// It deliberately never carries the underlying cause, which could quote
// secret bytes — the no-secret-in-errors invariant is enforced at this
// boundary, matching iacconnstore's posture.
var errCredMarshalFailed = errors.New("connectors: cred-marshal-failed")

// ErrCredentialsNotFound is returned by CredentialStore.Get when no
// sealed credentials exist for the connector. Callers errors.Is against
// it to distinguish "never configured" from a decrypt or storage error.
var ErrCredentialsNotFound = errors.New("connectors: credentials not found")

// MarshalCredentials serializes creds to JSON and seals the payload with
// the supplied credstore.Key, returning a single opaque blob laid out as
// [12-byte nonce || ciphertext] — the same convention as
// iacconnstore.MarshalGitHubPATCreds. The blob is what a credential
// substrate persists.
//
// Returns errCredMarshalFailed (errors.Is-comparable) on any failure.
// The error never carries the secret bytes, the JSON output, or the
// cipher's underlying message.
func MarshalCredentials(creds ConnectorCredentials, key *credstore.Key) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("%w: key is required", errCredMarshalFailed)
	}
	plaintext, err := json.Marshal(creds)
	if err != nil {
		// Defensive: cannot happen for this struct, but if it ever did
		// we MUST NOT propagate err (it may quote secret bytes).
		return nil, errCredMarshalFailed
	}
	ciphertext, nonce, err := key.Seal(plaintext)
	if err != nil {
		return nil, errCredMarshalFailed
	}
	if len(nonce) != nonceLen {
		return nil, errCredMarshalFailed
	}
	blob := make([]byte, 0, nonceLen+len(ciphertext))
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// UnmarshalCredentials unpacks blob (the [nonce || ciphertext] layout
// MarshalCredentials produces), decrypts it with key, and JSON-decodes
// the result back into ConnectorCredentials. Any decrypt failure (wrong
// key, tampered ciphertext, truncated blob) yields an error whose
// message contains "decrypt" and which never carries secret bytes.
func UnmarshalCredentials(blob []byte, key *credstore.Key) (*ConnectorCredentials, error) {
	if key == nil {
		return nil, errors.New("connectors: UnmarshalCredentials: key is required")
	}
	if len(blob) < nonceLen {
		return nil, errors.New("connectors: decrypt failed: blob shorter than nonce length")
	}
	nonce := blob[:nonceLen]
	ciphertext := blob[nonceLen:]
	plaintext, err := key.Open(ciphertext, nonce)
	if err != nil {
		// credstore.Key.Open already wraps with "decrypt failed"; keep
		// that signal and add our package prefix.
		return nil, fmt.Errorf("connectors: %w", err)
	}
	var creds ConnectorCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		// No-leak posture: do not propagate the json error (it may quote
		// plaintext bytes).
		return nil, errors.New("connectors: decrypt failed: plaintext is not valid ConnectorCredentials JSON")
	}
	return &creds, nil
}

// CredentialStore persists connector secrets sealed at rest, keyed by
// connector ID. It is the connector-scoped analog of the discovery
// credstore: the same sealing primitive, a store surface shaped for the
// connector framework's needs.
//
// Implementations must be safe for concurrent use. Get is the only path
// that returns plaintext; Has lets callers render a "has_secret:
// true/false" posture without unsealing.
type CredentialStore interface {
	// Put seals creds for connectorID and stores the blob, replacing any
	// existing credentials for that ID.
	Put(ctx context.Context, connectorID string, creds ConnectorCredentials) error

	// Get unseals and returns the credentials for connectorID, or
	// ErrCredentialsNotFound (errors.Is-comparable) when none are
	// stored.
	Get(ctx context.Context, connectorID string) (*ConnectorCredentials, error)

	// Has reports whether sealed credentials exist for connectorID
	// without unsealing them.
	Has(ctx context.Context, connectorID string) (bool, error)

	// Delete removes the credentials for connectorID. Idempotent:
	// deleting a non-existent entry is not an error.
	Delete(ctx context.Context, connectorID string) error
}

// memoryCredentialStore is the in-memory CredentialStore used by tests
// and ephemeral/dev deployments. It stores SEALED blobs — never
// plaintext — so its at-rest posture matches a persistent backend: the
// map holds ciphertext keyed by connector ID, and plaintext exists only
// transiently inside Put/Get.
type memoryCredentialStore struct {
	mu     sync.RWMutex
	key    *credstore.Key
	sealed map[string][]byte
}

// NewMemoryCredentialStore returns an in-memory CredentialStore that
// seals with the supplied credstore.Key. The key is required — a nil key
// is a programming error and yields an error at Put/Get time, matching
// the substrate's "refuses to run without a key" posture.
func NewMemoryCredentialStore(key *credstore.Key) CredentialStore {
	return &memoryCredentialStore{
		key:    key,
		sealed: make(map[string][]byte),
	}
}

// Put seals creds and stores the blob. Returns an error if the store has
// no key or connectorID is empty.
func (m *memoryCredentialStore) Put(_ context.Context, connectorID string, creds ConnectorCredentials) error {
	if connectorID == "" {
		return errors.New("connectors: Put: connectorID is required")
	}
	blob, err := MarshalCredentials(creds, m.key)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sealed[connectorID] = blob
	return nil
}

// Get unseals and returns the stored credentials, or
// ErrCredentialsNotFound.
func (m *memoryCredentialStore) Get(_ context.Context, connectorID string) (*ConnectorCredentials, error) {
	if connectorID == "" {
		return nil, errors.New("connectors: Get: connectorID is required")
	}
	m.mu.RLock()
	blob, ok := m.sealed[connectorID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrCredentialsNotFound
	}
	return UnmarshalCredentials(blob, m.key)
}

// Has reports whether a sealed blob exists for connectorID.
func (m *memoryCredentialStore) Has(_ context.Context, connectorID string) (bool, error) {
	if connectorID == "" {
		return false, errors.New("connectors: Has: connectorID is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.sealed[connectorID]
	return ok, nil
}

// Delete removes the sealed blob for connectorID. Idempotent.
func (m *memoryCredentialStore) Delete(_ context.Context, connectorID string) error {
	if connectorID == "" {
		return errors.New("connectors: Delete: connectorID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sealed, connectorID)
	return nil
}
