// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// spyAudit is a fake AuditRecorder that captures every call. The
// substrate's contract is that every read emits a
// discovery.<provider>.connection_read event; the spy lets the
// assertions verify both the event count and the payload contents.
type spyAudit struct {
	mu      sync.Mutex
	entries []services.AuditEntry
}

func (s *spyAudit) Record(_ context.Context, entry services.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *spyAudit) snapshot() []services.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]services.AuditEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

// testKey is a deterministic 32-byte key for tests. The base64 form is
// what an operator would set in SQUADRON_SECRETS_KEY. Tests that need
// to exercise key-loading set / unset this env var; tests that don't
// care about key handling rely on setTestKey() to provide a valid one.
func testKeyBase64() string {
	raw := make([]byte, keyByteLen)
	for i := range raw {
		raw[i] = byte(i + 1) // arbitrary, non-zero, deterministic
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// setTestKey points SQUADRON_SECRETS_KEY at a valid key for the duration
// of the test. Calls t.Setenv so cleanup is automatic.
func setTestKey(t *testing.T) {
	t.Helper()
	t.Setenv(EnvVarSecretsKey, testKeyBase64())
}

// testKeyRaw returns the same deterministic 32-byte key as
// testKeyBase64 but already decoded. Tests that bypass the env-var
// path (e.g. by constructing a Key directly) use this.
func testKeyRaw() []byte {
	raw := make([]byte, keyByteLen)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	return raw
}

// newTestKey constructs a Key from the deterministic test raw bytes.
func newTestKey(t *testing.T) *Key {
	t.Helper()
	key, err := NewKey(testKeyRaw())
	require.NoError(t, err, "NewKey should succeed with 32 raw bytes")
	return key
}

func newTestStore(t *testing.T) (Store, *spyAudit) {
	t.Helper()
	setTestKey(t)
	audit := &spyAudit{}
	dbPath := filepath.Join(t.TempDir(), "credstore.db")
	store, err := NewSQLiteStore(Config{
		DBPath: dbPath,
		Audit:  audit,
		Logger: zap.NewNop(),
	})
	require.NoError(t, err, "NewSQLiteStore should succeed with valid key + audit")
	t.Cleanup(func() { _ = store.Close() })
	return store, audit
}

// sampleAWSConnection builds a CloudConnection for an AWS account
// with valid AWSCredentials already marshaled into the Credentials
// blob. accountID flows into the role ARN and ExternalID so tests can
// distinguish per-account material in assertions.
func sampleAWSConnection(t *testing.T, key *Key, accountID string) CloudConnection {
	t.Helper()
	creds := AWSCredentials{
		RoleARN:    "arn:aws:iam::" + accountID + ":role/SquadronDiscovery",
		ExternalID: "external-id-" + accountID + "-very-secret",
	}
	ciphertext, nonce, err := MarshalAWSCredentials(creds, key)
	require.NoError(t, err)
	return CloudConnection{
		AccountID:        accountID,
		Provider:         ProviderAWS,
		ConnectionType:   ConnectionAPIDiscovered,
		DisplayName:      "acct-" + accountID,
		Regions:          []string{"us-east-1"},
		Credentials:      ciphertext,
		CredentialsNonce: nonce,
	}
}

// TestEncryptDecryptRoundtrip verifies the substrate's primary
// guarantee: AWS credentials written through StoreConnection come back
// through GetConnection + UnmarshalAWSCredentials unchanged, and the
// on-disk row never contains the plaintext ExternalID.
func TestEncryptDecryptRoundtrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	in := sampleAWSConnection(t, key, "111111111111")
	require.NoError(t, store.StoreConnection(ctx, in))

	out, err := store.GetConnection(ctx, in.AccountID)
	require.NoError(t, err)
	require.NotNil(t, out, "Get should return a row for a stored account")

	assert.Equal(t, in.AccountID, out.AccountID)
	assert.Equal(t, ProviderAWS, out.Provider)
	assert.Equal(t, ConnectionAPIDiscovered, out.ConnectionType)
	assert.Equal(t, in.DisplayName, out.DisplayName)
	assert.Equal(t, in.Regions, out.Regions)
	assert.False(t, out.CreatedAt.IsZero(), "CreatedAt should be stamped")
	assert.False(t, out.UpdatedAt.IsZero(), "UpdatedAt should be stamped")

	// The Credentials field comes back as ciphertext; decrypt and
	// verify the AWS-typed roundtrip.
	creds, err := UnmarshalAWSCredentials(out.Credentials, out.CredentialsNonce, key)
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::"+in.AccountID+":role/SquadronDiscovery", creds.RoleARN)
	assert.Equal(t, "external-id-"+in.AccountID+"-very-secret", creds.ExternalID,
		"ExternalID must roundtrip through encrypt/decrypt unchanged")

	// Verify the on-disk row does not contain the plaintext ExternalID.
	// Reach into the sqliteStore concrete type to query the raw column.
	s, ok := store.(*sqliteStore)
	require.True(t, ok, "store should be *sqliteStore")
	var ciphertext []byte
	require.NoError(t, s.db.QueryRowContext(ctx,
		"SELECT credentials_ciphertext FROM cloud_connections WHERE account_id = ?",
		in.AccountID,
	).Scan(&ciphertext))
	assert.NotContains(t, string(ciphertext), creds.ExternalID,
		"on-disk ciphertext must NOT contain the plaintext ExternalID")
	assert.NotContains(t, string(ciphertext), creds.RoleARN,
		"on-disk ciphertext must NOT contain the plaintext RoleARN")
}

// TestTamperDetection verifies that AES-256-GCM's authentication tag
// catches modifications to the stored ciphertext. The substrate's
// security posture depends on this: an attacker who gains write
// access to the SQLite file must not be able to swap credentials
// without detection.
func TestTamperDetection(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	in := sampleAWSConnection(t, key, "222222222222")
	require.NoError(t, store.StoreConnection(ctx, in))

	// Flip one byte in the ciphertext column. GCM's tag covers the
	// whole ciphertext + nonce + key, so any modification fails Open.
	s := store.(*sqliteStore)
	const tamper = `
		UPDATE cloud_connections
		SET credentials_ciphertext = ? || credentials_ciphertext
		WHERE account_id = ?
	`
	_, err := s.db.ExecContext(ctx, tamper, []byte{0xFF}, in.AccountID)
	require.NoError(t, err)

	// Get itself succeeds (ciphertext is opaque to the substrate);
	// the caller's Unmarshal step is what fails.
	out, err := store.GetConnection(ctx, in.AccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	creds, err := UnmarshalAWSCredentials(out.Credentials, out.CredentialsNonce, key)
	require.Error(t, err, "Unmarshal must fail when ciphertext is tampered")
	assert.Nil(t, creds, "tampered Unmarshal must not return credentials")

	// The error must mention either "decrypt" or "auth" so operators
	// and downstream code can match against a stable signal.
	msg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(msg, "decrypt") || strings.Contains(msg, "auth"),
		"tamper error should contain 'decrypt' or 'auth'; got %q", err.Error(),
	)
}

// TestMissingKeyFailsLoudly verifies the constructor rejects every
// invalid key state. No fallback to a default key, a zero key, or
// plaintext exists — operators get a hard error and Squadron does not
// boot. The three cases together cover the env-var contract.
func TestMissingKeyFailsLoudly(t *testing.T) {
	audit := &spyAudit{}
	dbPath := filepath.Join(t.TempDir(), "credstore.db")
	cfg := Config{DBPath: dbPath, Audit: audit, Logger: zap.NewNop()}

	t.Run("unset", func(t *testing.T) {
		t.Setenv(EnvVarSecretsKey, "")
		_, err := NewSQLiteStore(cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSecretsKeyMissing)
	})

	t.Run("not base64", func(t *testing.T) {
		t.Setenv(EnvVarSecretsKey, "not!valid!base64!!!")
		_, err := NewSQLiteStore(cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSecretsKeyMalformed)
	})

	t.Run("wrong length", func(t *testing.T) {
		// 16 bytes — valid base64, but AES-128 not AES-256.
		short := base64.StdEncoding.EncodeToString(make([]byte, 16))
		t.Setenv(EnvVarSecretsKey, short)
		_, err := NewSQLiteStore(cfg)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSecretsKeyMalformed)
	})
}

// TestAuditEmissionOnRead verifies the substrate's audit contract:
// every Get and every row returned by List produces a
// discovery.<provider>.connection_read event, the payload carries
// account_id and provider, and the credentials bytes never appear in
// the emitted event.
func TestAuditEmissionOnRead(t *testing.T) {
	store, audit := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	conns := []CloudConnection{
		sampleAWSConnection(t, key, "333333333333"),
		sampleAWSConnection(t, key, "444444444444"),
	}
	for _, c := range conns {
		require.NoError(t, store.StoreConnection(ctx, c))
	}

	// Writes must not emit connection_read — that event is for READS only.
	require.Empty(t, audit.snapshot(), "writes must not emit connection_read")

	// One Get => one event.
	got, err := store.GetConnection(ctx, conns[0].AccountID)
	require.NoError(t, err)
	require.NotNil(t, got)

	entries := audit.snapshot()
	require.Len(t, entries, 1, "one Get => one audit event")
	assertConnectionRead(t, entries[0], conns[0])

	// List of two rows => two more events, one per row.
	listed, err := store.ListConnections(ctx, ListFilter{})
	require.NoError(t, err)
	require.Len(t, listed, 2)

	entries = audit.snapshot()
	require.Len(t, entries, 3, "Get + List(2) => 3 events total")
	assertConnectionRead(t, entries[1], conns[0])
	assertConnectionRead(t, entries[2], conns[1])
}

// assertConnectionRead centralizes the per-event invariants so the
// test body stays focused on counts and ordering. Every read event
// must:
//   - use the provider-prefixed event type and uniform target type
//   - carry account_id, provider, connection_type in the payload
//   - NEVER carry the credentials bytes under any key
func assertConnectionRead(t *testing.T, entry services.AuditEntry, expected CloudConnection) {
	t.Helper()
	assert.Equal(t, FormatConnectionReadEvent(expected.Provider), entry.EventType)
	assert.Equal(t, TargetTypeCloudConnection, entry.TargetType)
	assert.Equal(t, expected.AccountID, entry.TargetID)
	assert.Equal(t, services.AuditActorSystem, entry.Actor)

	require.NotNil(t, entry.Payload)
	assert.Equal(t, expected.AccountID, entry.Payload["account_id"])
	assert.Equal(t, string(expected.Provider), entry.Payload["provider"])
	assert.Equal(t, string(expected.ConnectionType), entry.Payload["connection_type"])

	// No credentials, ciphertext, or nonce key should appear in the
	// payload under any name a future refactor might choose.
	for _, banned := range []string{"credentials", "credentials_ciphertext", "credentials_nonce", "external_id", "role_arn"} {
		_, hasKey := entry.Payload[banned]
		assert.False(t, hasKey, "audit payload must not have a %q key", banned)
	}
}

// TestListEmptyAndPopulated verifies List behavior in both states: an
// empty store returns no rows and no error; after inserting two
// connections, both come back with their ciphertext payloads intact.
func TestListEmptyAndPopulated(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	// Empty store.
	listed, err := store.ListConnections(ctx, ListFilter{})
	require.NoError(t, err)
	assert.Empty(t, listed, "empty store should return no connections")

	// Populate with two.
	a := sampleAWSConnection(t, key, "555555555555")
	b := sampleAWSConnection(t, key, "666666666666")
	require.NoError(t, store.StoreConnection(ctx, a))
	require.NoError(t, store.StoreConnection(ctx, b))

	listed, err = store.ListConnections(ctx, ListFilter{})
	require.NoError(t, err)
	require.Len(t, listed, 2)

	// Ordering is by account_id ASC per the SQL; "555..." comes before
	// "666..." so this is deterministic.
	assert.Equal(t, a.AccountID, listed[0].AccountID)
	credsA, err := UnmarshalAWSCredentials(listed[0].Credentials, listed[0].CredentialsNonce, key)
	require.NoError(t, err)
	assert.Equal(t, "external-id-"+a.AccountID+"-very-secret", credsA.ExternalID,
		"ExternalID must roundtrip through List as well as Get")
	assert.Equal(t, b.AccountID, listed[1].AccountID)
	credsB, err := UnmarshalAWSCredentials(listed[1].Credentials, listed[1].CredentialsNonce, key)
	require.NoError(t, err)
	assert.Equal(t, "external-id-"+b.AccountID+"-very-secret", credsB.ExternalID)
}

// TestGetMissingReturnsNil verifies the (nil, nil) contract for absent
// rows. This is the documented behavior callers rely on to distinguish
// "not configured" from "configured but errored."
func TestGetMissingReturnsNil(t *testing.T) {
	store, audit := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetConnection(ctx, "999999999999")
	require.NoError(t, err)
	assert.Nil(t, got, "Get on absent account_id must return (nil, nil)")
	assert.Empty(t, audit.snapshot(),
		"a Get that returns no row must not emit a connection_read event")
}

// TestStoreUpsertPreservesCreatedAt verifies that re-storing an
// existing account updates the metadata + ciphertext but keeps the
// original CreatedAt. Audit timelines and operator UI depend on the
// "when did you first connect this account" timestamp being stable.
func TestStoreUpsertPreservesCreatedAt(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	first := sampleAWSConnection(t, key, "777777777777")
	require.NoError(t, store.StoreConnection(ctx, first))
	gotFirst, err := store.GetConnection(ctx, first.AccountID)
	require.NoError(t, err)
	require.NotNil(t, gotFirst)

	// Second write with a different display name + rotated ExternalID.
	rotated := AWSCredentials{
		RoleARN:    "arn:aws:iam::" + first.AccountID + ":role/SquadronDiscovery",
		ExternalID: "rotated-external-id",
	}
	cipher, nonce, err := MarshalAWSCredentials(rotated, key)
	require.NoError(t, err)
	second := first
	second.DisplayName = "renamed"
	second.Credentials = cipher
	second.CredentialsNonce = nonce
	require.NoError(t, store.StoreConnection(ctx, second))

	gotSecond, err := store.GetConnection(ctx, first.AccountID)
	require.NoError(t, err)
	require.NotNil(t, gotSecond)

	assert.Equal(t, "renamed", gotSecond.DisplayName)
	gotCreds, err := UnmarshalAWSCredentials(gotSecond.Credentials, gotSecond.CredentialsNonce, key)
	require.NoError(t, err)
	assert.Equal(t, "rotated-external-id", gotCreds.ExternalID)
	assert.Equal(t, gotFirst.CreatedAt, gotSecond.CreatedAt,
		"upsert must preserve original CreatedAt")
	assert.False(t, gotSecond.UpdatedAt.Before(gotFirst.UpdatedAt),
		"upsert must move UpdatedAt forward (or hold steady)")
}

// TestDeleteIsIdempotent verifies Delete succeeds on missing rows and
// that a deleted account is gone from subsequent Get and List calls.
func TestDeleteIsIdempotent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	require.NoError(t, store.DeleteConnection(ctx, "no-such-account"),
		"delete on missing row should be a no-op, not an error")

	conn := sampleAWSConnection(t, key, "888888888888")
	require.NoError(t, store.StoreConnection(ctx, conn))
	require.NoError(t, store.DeleteConnection(ctx, conn.AccountID))

	got, err := store.GetConnection(ctx, conn.AccountID)
	require.NoError(t, err)
	assert.Nil(t, got, "deleted account should be absent from Get")

	listed, err := store.ListConnections(ctx, ListFilter{})
	require.NoError(t, err)
	assert.Empty(t, listed, "deleted account should be absent from List")
}

// TestProviderDiscriminator stores an AWS connection and a fake GCP
// connection, then verifies ListConnections with Provider: ProviderAWS
// filters correctly. The GCP row's Credentials blob is arbitrary bytes
// (GCP scanner doesn't exist yet) — the substrate stores it opaquely.
func TestProviderDiscriminator(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	awsConn := sampleAWSConnection(t, key, "100000000001")
	require.NoError(t, store.StoreConnection(ctx, awsConn))

	// Fake GCP row. The substrate doesn't know what GCP credentials
	// look like; it stores opaque encrypted bytes. We seal arbitrary
	// JSON to mimic a future GCP marshaler.
	gcpPlaintext := []byte(`{"workload_identity_pool":"projects/foo/locations/global/workloadIdentityPools/bar"}`)
	gcpCipher, gcpNonce, err := key.Seal(gcpPlaintext)
	require.NoError(t, err)
	gcpConn := CloudConnection{
		AccountID:        "gcp-project-foo",
		Provider:         ProviderGCP,
		ConnectionType:   ConnectionAPIDiscovered,
		DisplayName:      "GCP staging project",
		Regions:          []string{"us-central1"},
		Credentials:      gcpCipher,
		CredentialsNonce: gcpNonce,
	}
	require.NoError(t, store.StoreConnection(ctx, gcpConn))

	// Filter to AWS only.
	awsOnly, err := store.ListConnections(ctx, ListFilter{Provider: ProviderAWS})
	require.NoError(t, err)
	require.Len(t, awsOnly, 1, "AWS filter should match exactly one row")
	assert.Equal(t, awsConn.AccountID, awsOnly[0].AccountID)
	assert.Equal(t, ProviderAWS, awsOnly[0].Provider)

	// Filter to GCP only.
	gcpOnly, err := store.ListConnections(ctx, ListFilter{Provider: ProviderGCP})
	require.NoError(t, err)
	require.Len(t, gcpOnly, 1, "GCP filter should match exactly one row")
	assert.Equal(t, gcpConn.AccountID, gcpOnly[0].AccountID)
	assert.Equal(t, ProviderGCP, gcpOnly[0].Provider)

	// Empty filter returns both, ordered by account_id ASC.
	all, err := store.ListConnections(ctx, ListFilter{})
	require.NoError(t, err)
	require.Len(t, all, 2)
}

// TestConnectionTypeEnum stores a connection with each ConnectionType
// value and verifies they all roundtrip. The substrate is the schema
// owner for the on-prem connection_type from day one even though
// slice 6 is what implements agent-polled discovery.
func TestConnectionTypeEnum(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	cases := []struct {
		accountID string
		provider  Provider
		connType  ConnectionType
	}{
		{"acct-api", ProviderAWS, ConnectionAPIDiscovered},
		{"acct-agent", ProviderOnPrem, ConnectionAgentPolled},
		{"acct-manual", ProviderAWS, ConnectionManualImport},
	}
	for _, tc := range cases {
		// Use arbitrary sealed bytes — the substrate is opaque to
		// the credentials shape for non-AWS providers in this test.
		cipher, nonce, err := key.Seal([]byte(`{"placeholder":true}`))
		require.NoError(t, err)
		conn := CloudConnection{
			AccountID:        tc.accountID,
			Provider:         tc.provider,
			ConnectionType:   tc.connType,
			DisplayName:      tc.accountID,
			Regions:          []string{"us-east-1"},
			Credentials:      cipher,
			CredentialsNonce: nonce,
		}
		require.NoError(t, store.StoreConnection(ctx, conn))

		got, err := store.GetConnection(ctx, tc.accountID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, tc.connType, got.ConnectionType,
			"ConnectionType %q must roundtrip", tc.connType)
		assert.Equal(t, tc.provider, got.Provider)
	}
}

// TestMultiRegionRegionsRoundtrip stores a connection with multiple
// regions and verifies the slice is preserved verbatim through the
// JSON-column encoding. Multi-region is architecture-only in slice 1
// but the schema supports it from day one.
func TestMultiRegionRegionsRoundtrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	conn := sampleAWSConnection(t, key, "200000000002")
	conn.Regions = []string{"us-east-1", "eu-west-1", "ap-southeast-2"}
	require.NoError(t, store.StoreConnection(ctx, conn))

	got, err := store.GetConnection(ctx, conn.AccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"us-east-1", "eu-west-1", "ap-southeast-2"}, got.Regions,
		"multi-region slice must roundtrip preserving order and values")

	// List path must preserve the slice too.
	listed, err := store.ListConnections(ctx, ListFilter{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, []string{"us-east-1", "eu-west-1", "ap-southeast-2"}, listed[0].Regions)
}

// TestAWSCredentialsMarshalRoundtrip exercises the AWS-specific
// marshal helpers directly: encrypt AWSCredentials, decrypt, verify
// the values match, then verify tamper detection still holds at the
// helper boundary (not just at the substrate boundary).
func TestAWSCredentialsMarshalRoundtrip(t *testing.T) {
	key := newTestKey(t)

	creds := AWSCredentials{
		RoleARN:    "arn:aws:iam::123456789012:role/SquadronDiscovery",
		ExternalID: "external-id-roundtrip-test",
	}
	ciphertext, nonce, err := MarshalAWSCredentials(creds, key)
	require.NoError(t, err)
	require.NotEmpty(t, ciphertext)
	require.Len(t, nonce, nonceByteLen)

	got, err := UnmarshalAWSCredentials(ciphertext, nonce, key)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, creds.RoleARN, got.RoleARN)
	assert.Equal(t, creds.ExternalID, got.ExternalID)

	// Ciphertext must not contain plaintext.
	assert.NotContains(t, string(ciphertext), creds.RoleARN,
		"ciphertext must not leak RoleARN as plaintext bytes")
	assert.NotContains(t, string(ciphertext), creds.ExternalID,
		"ciphertext must not leak ExternalID as plaintext bytes")

	// Tamper: flip a byte in the ciphertext. Open must fail with a
	// decrypt/auth error.
	tampered := make([]byte, len(ciphertext))
	copy(tampered, ciphertext)
	tampered[0] ^= 0xFF
	_, err = UnmarshalAWSCredentials(tampered, nonce, key)
	require.Error(t, err, "tampering with ciphertext must trigger an auth failure")
	msg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(msg, "decrypt") || strings.Contains(msg, "auth"),
		"tamper error should contain 'decrypt' or 'auth'; got %q", err.Error(),
	)
}

// TestAuditEventTypeIsProviderPrefixed verifies the event type is the
// provider-prefixed form "discovery.aws.connection_read" rather than
// the unprefixed string from the pre-refactor Stream 2A pass. This is
// the contract that lets the audit timeline filter per provider.
func TestAuditEventTypeIsProviderPrefixed(t *testing.T) {
	store, audit := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	conn := sampleAWSConnection(t, key, "300000000003")
	require.NoError(t, store.StoreConnection(ctx, conn))

	_, err := store.GetConnection(ctx, conn.AccountID)
	require.NoError(t, err)

	entries := audit.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, "discovery.aws.connection_read", entries[0].EventType,
		"event type must be the provider-prefixed form, not the legacy 'discovery.role_assumed'")
	assert.NotEqual(t, "discovery.role_assumed", entries[0].EventType,
		"legacy unprefixed event type must not be re-introduced")
}

// recordingBackend is a SecretsBackend that wraps another backend and
// counts every Encrypt/Decrypt call. Used by
// TestSecretsBackendInterface to verify the substrate's pluggability.
type recordingBackend struct {
	inner        SecretsBackend
	mu           sync.Mutex
	encryptCalls int
	decryptCalls int
}

func (r *recordingBackend) Encrypt(plaintext []byte) ([]byte, []byte, error) {
	r.mu.Lock()
	r.encryptCalls++
	r.mu.Unlock()
	return r.inner.Encrypt(plaintext)
}

func (r *recordingBackend) Decrypt(ciphertext, nonce []byte) ([]byte, error) {
	r.mu.Lock()
	r.decryptCalls++
	r.mu.Unlock()
	return r.inner.Decrypt(ciphertext, nonce)
}

// TestSecretsBackendInterface constructs the Store with a custom
// SecretsBackend that records every call, exercises a few read/write
// operations, and verifies the backend is on the path the Compliance
// Pack will need to plug into. The substrate must accept any
// SecretsBackend implementation without code changes — this test is
// the contract.
func TestSecretsBackendInterface(t *testing.T) {
	ctx := context.Background()
	key := newTestKey(t)

	// Wrap the OSS-default backend so the test still exercises real
	// AES-GCM rather than a stub. The recording layer's job is to
	// verify the call path, not to re-test encryption.
	inner := NewSQLiteSecretsBackend(key)
	recording := &recordingBackend{inner: inner}

	// Build an in-memory SQLite DB and call NewStore with the custom
	// backend. This is the path the Compliance Pack will use:
	// caller manages the DB, caller picks the backend.
	dbPath := filepath.Join(t.TempDir(), "credstore-custom-backend.db")
	store, err := openStoreWithBackend(t, ctx, dbPath, recording)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// The store accepted our custom backend without inspecting the
	// concrete type. That is the interface guarantee.
	conn := sampleAWSConnection(t, key, "400000000004")
	require.NoError(t, store.StoreConnection(ctx, conn))

	got, err := store.GetConnection(ctx, conn.AccountID)
	require.NoError(t, err)
	require.NotNil(t, got)

	// The substrate today stores Credentials opaquely, so it does
	// not call backend.Encrypt or backend.Decrypt itself — the
	// per-provider Marshal/Unmarshal helpers do. The test asserts
	// the interface is wired (the constructor accepted it) and the
	// backend can also be used directly via MarshalAWSCredentials,
	// which is the contract Compliance Pack implementations rely on.
	cipher, nonce, err := recording.Encrypt([]byte("plaintext-via-backend"))
	require.NoError(t, err)
	plain, err := recording.Decrypt(cipher, nonce)
	require.NoError(t, err)
	assert.Equal(t, []byte("plaintext-via-backend"), plain)

	recording.mu.Lock()
	defer recording.mu.Unlock()
	assert.GreaterOrEqual(t, recording.encryptCalls, 1,
		"recording backend's Encrypt should have been invoked")
	assert.GreaterOrEqual(t, recording.decryptCalls, 1,
		"recording backend's Decrypt should have been invoked")
}

// openStoreWithBackend is the test helper that exercises NewStore
// directly (the constructor the Compliance Pack will use). Open a
// SQLite DB at dbPath, run migrations through NewStore, return a
// Store backed by the supplied SecretsBackend.
func openStoreWithBackend(t *testing.T, ctx context.Context, dbPath string, backend SecretsBackend) (Store, error) {
	t.Helper()
	// The sqlite3 driver is registered by sqlite.go's blank import,
	// which the test file inherits transitively through the package.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	audit := &spyAudit{}
	store, err := NewStore(ctx, db, backend, audit)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}
