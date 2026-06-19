// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
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
// discovery.role_assumed event; the spy lets the assertions verify
// both the event count and the payload contents.
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

func sampleConnection(accountID string) AWSConnection {
	return AWSConnection{
		AccountID:   accountID,
		RoleARN:     "arn:aws:iam::" + accountID + ":role/SquadronDiscovery",
		ExternalID:  "external-id-" + accountID + "-very-secret",
		DisplayName: "acct-" + accountID,
		Region:      "us-east-1",
	}
}

// TestEncryptDecryptRoundtrip verifies the substrate's primary
// guarantee: an ExternalID written through StoreAWSConnection comes
// back through GetAWSConnection unchanged, and the on-disk row never
// contains the plaintext.
func TestEncryptDecryptRoundtrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	in := sampleConnection("111111111111")
	require.NoError(t, store.StoreAWSConnection(ctx, in))

	out, err := store.GetAWSConnection(ctx, in.AccountID)
	require.NoError(t, err)
	require.NotNil(t, out, "Get should return a row for a stored account")

	assert.Equal(t, in.AccountID, out.AccountID)
	assert.Equal(t, in.RoleARN, out.RoleARN)
	assert.Equal(t, in.ExternalID, out.ExternalID,
		"ExternalID must roundtrip through encrypt/decrypt unchanged")
	assert.Equal(t, in.DisplayName, out.DisplayName)
	assert.Equal(t, in.Region, out.Region)
	assert.False(t, out.CreatedAt.IsZero(), "CreatedAt should be stamped")
	assert.False(t, out.UpdatedAt.IsZero(), "UpdatedAt should be stamped")

	// Verify the on-disk row does not contain the plaintext. Reach into
	// the sqliteStore concrete type to query the raw ciphertext column.
	s, ok := store.(*sqliteStore)
	require.True(t, ok, "store should be *sqliteStore")
	var ciphertext []byte
	require.NoError(t, s.db.QueryRowContext(ctx,
		"SELECT external_id_ciphertext FROM aws_connections WHERE account_id = ?",
		in.AccountID,
	).Scan(&ciphertext))
	assert.NotContains(t, string(ciphertext), in.ExternalID,
		"on-disk ciphertext must NOT contain the plaintext ExternalID")
}

// TestTamperDetection verifies that AES-256-GCM's authentication tag
// catches modifications to the stored ciphertext. The substrate's
// security posture depends on this: an attacker who gains write
// access to the SQLite file must not be able to swap an ExternalID
// without detection.
func TestTamperDetection(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	in := sampleConnection("222222222222")
	require.NoError(t, store.StoreAWSConnection(ctx, in))

	// Flip one byte in the ciphertext column. GCM's tag covers the
	// whole ciphertext + nonce + key, so any modification fails Open.
	s := store.(*sqliteStore)
	const tamper = `
		UPDATE aws_connections
		SET external_id_ciphertext = ? || external_id_ciphertext
		WHERE account_id = ?
	`
	_, err := s.db.ExecContext(ctx, tamper, []byte{0xFF}, in.AccountID)
	require.NoError(t, err)

	out, err := store.GetAWSConnection(ctx, in.AccountID)
	require.Error(t, err, "Get must fail when ciphertext is tampered")
	assert.Nil(t, out, "tampered Get must not return a connection")

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
// discovery.role_assumed event, the payload carries account_id and
// role_arn, and the ExternalID never appears in the emitted event.
func TestAuditEmissionOnRead(t *testing.T) {
	store, audit := newTestStore(t)
	ctx := context.Background()

	conns := []AWSConnection{
		sampleConnection("333333333333"),
		sampleConnection("444444444444"),
	}
	for _, c := range conns {
		require.NoError(t, store.StoreAWSConnection(ctx, c))
	}

	// Writes must not emit role_assumed — that event is for READS only.
	require.Empty(t, audit.snapshot(), "writes must not emit discovery.role_assumed")

	// One Get => one event.
	got, err := store.GetAWSConnection(ctx, conns[0].AccountID)
	require.NoError(t, err)
	require.NotNil(t, got)

	entries := audit.snapshot()
	require.Len(t, entries, 1, "one Get => one audit event")
	assertRoleAssumed(t, entries[0], conns[0])

	// List of two rows => two more events, one per row.
	listed, err := store.ListAWSConnections(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)

	entries = audit.snapshot()
	require.Len(t, entries, 3, "Get + List(2) => 3 events total")
	assertRoleAssumed(t, entries[1], conns[0])
	assertRoleAssumed(t, entries[2], conns[1])
}

// assertRoleAssumed centralizes the per-event invariants so the test
// body stays focused on counts and ordering. Every read event must:
//   - use the canonical event type and target type
//   - carry account_id and role_arn in the payload
//   - NEVER carry the ExternalID under any key
func assertRoleAssumed(t *testing.T, entry services.AuditEntry, expected AWSConnection) {
	t.Helper()
	assert.Equal(t, EventTypeRoleAssumed, entry.EventType)
	assert.Equal(t, TargetTypeAWSConnection, entry.TargetType)
	assert.Equal(t, expected.AccountID, entry.TargetID)
	assert.Equal(t, services.AuditActorSystem, entry.Actor)

	require.NotNil(t, entry.Payload)
	assert.Equal(t, expected.AccountID, entry.Payload["account_id"])
	assert.Equal(t, expected.RoleARN, entry.Payload["role_arn"])

	// Scan every payload value for the ExternalID string. Catches both
	// a literal "external_id" key and any future refactor that
	// accidentally embeds the secret in a nested map or message.
	for k, v := range entry.Payload {
		if s, ok := v.(string); ok {
			assert.NotEqual(t, expected.ExternalID, s,
				"audit payload key %q must not contain the ExternalID", k)
		}
	}
	_, hasKey := entry.Payload["external_id"]
	assert.False(t, hasKey, "audit payload must not have an external_id key")
}

// TestListEmptyAndPopulated verifies List behavior in both states: an
// empty store returns no rows and no error; after inserting two
// connections, both come back with decrypted ExternalIDs.
func TestListEmptyAndPopulated(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Empty store.
	listed, err := store.ListAWSConnections(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed, "empty store should return no connections")

	// Populate with two.
	a := sampleConnection("555555555555")
	b := sampleConnection("666666666666")
	require.NoError(t, store.StoreAWSConnection(ctx, a))
	require.NoError(t, store.StoreAWSConnection(ctx, b))

	listed, err = store.ListAWSConnections(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)

	// Ordering is by account_id ASC per the SQL; "555..." comes before
	// "666..." so this is deterministic.
	assert.Equal(t, a.AccountID, listed[0].AccountID)
	assert.Equal(t, a.ExternalID, listed[0].ExternalID,
		"ExternalID must roundtrip through List as well as Get")
	assert.Equal(t, b.AccountID, listed[1].AccountID)
	assert.Equal(t, b.ExternalID, listed[1].ExternalID)
}

// TestGetMissingReturnsNil verifies the (nil, nil) contract for absent
// rows. This is the documented behavior callers rely on to distinguish
// "not configured" from "configured but errored."
func TestGetMissingReturnsNil(t *testing.T) {
	store, audit := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetAWSConnection(ctx, "999999999999")
	require.NoError(t, err)
	assert.Nil(t, got, "Get on absent account_id must return (nil, nil)")
	assert.Empty(t, audit.snapshot(),
		"a Get that returns no row must not emit a role_assumed event")
}

// TestStoreUpsertPreservesCreatedAt verifies that re-storing an
// existing account updates the metadata + ciphertext but keeps the
// original CreatedAt. Audit timelines and operator UI depend on the
// "when did you first connect this account" timestamp being stable.
func TestStoreUpsertPreservesCreatedAt(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	first := sampleConnection("777777777777")
	require.NoError(t, store.StoreAWSConnection(ctx, first))
	gotFirst, err := store.GetAWSConnection(ctx, first.AccountID)
	require.NoError(t, err)
	require.NotNil(t, gotFirst)

	// Second write with a different display name + ExternalID.
	second := first
	second.DisplayName = "renamed"
	second.ExternalID = "rotated-external-id"
	require.NoError(t, store.StoreAWSConnection(ctx, second))

	gotSecond, err := store.GetAWSConnection(ctx, first.AccountID)
	require.NoError(t, err)
	require.NotNil(t, gotSecond)

	assert.Equal(t, "renamed", gotSecond.DisplayName)
	assert.Equal(t, "rotated-external-id", gotSecond.ExternalID)
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

	require.NoError(t, store.DeleteAWSConnection(ctx, "no-such-account"),
		"delete on missing row should be a no-op, not an error")

	conn := sampleConnection("888888888888")
	require.NoError(t, store.StoreAWSConnection(ctx, conn))
	require.NoError(t, store.DeleteAWSConnection(ctx, conn.AccountID))

	got, err := store.GetAWSConnection(ctx, conn.AccountID)
	require.NoError(t, err)
	assert.Nil(t, got, "deleted account should be absent from Get")

	listed, err := store.ListAWSConnections(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed, "deleted account should be absent from List")
}
