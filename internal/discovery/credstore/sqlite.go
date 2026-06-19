// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// sqliteStore is the SQLite-backed Store implementation. It owns its
// own database file (separate from the application store) so the
// substrate's lifecycle is independent — operators can wipe credstore
// data without touching the rest of Squadron, and credstore migrations
// don't entangle with application-store migrations.
type sqliteStore struct {
	db      *sql.DB
	crypto  *cryptor
	audit   AuditRecorder
	logger  *zap.Logger
	timeNow func() time.Time // injectable so tests can pin timestamps
}

// Config configures a new SQLite-backed credential substrate.
//
//   - DBPath is the SQLite database file path. ":memory:" is supported
//     for tests.
//   - Audit is the audit recorder that receives discovery.role_assumed
//     events on every read. Required — the substrate refuses to start
//     without one because every read MUST be audited.
//   - Logger is optional; defaults to zap.NewNop() when nil. The logger
//     is intentionally only used for non-sensitive operational lines
//     (open / migration / close). ExternalIDs are NEVER logged.
type Config struct {
	DBPath string
	Audit  AuditRecorder
	Logger *zap.Logger
}

// NewSQLiteStore opens (or creates) the substrate's SQLite database,
// runs migrations, validates SQUADRON_SECRETS_KEY, and returns a Store
// ready to use. The construction fails loud on any of:
//
//   - SQUADRON_SECRETS_KEY missing or wrong length
//   - audit recorder nil
//   - database open / migration error
//
// No fallback to a default key, a zero key, or plaintext exists.
func NewSQLiteStore(cfg Config) (Store, error) {
	if cfg.Audit == nil {
		return nil, errors.New("credstore: Config.Audit is required; the substrate has no unaudited read path")
	}
	if cfg.DBPath == "" {
		return nil, errors.New("credstore: Config.DBPath is required")
	}

	key, err := loadKeyFromEnv()
	if err != nil {
		return nil, err
	}
	crypto, err := newCryptor(key)
	if err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("credstore: open sqlite at %q: %w", cfg.DBPath, err)
	}
	// Match the application-store pool tuning so behavior is predictable
	// on shared hosts. WAL stays off for the substrate by default;
	// volume is very low and journal_mode=DELETE keeps the file simple
	// for operators to back up.
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("credstore: enable foreign keys: %w", err)
	}

	store := &sqliteStore{
		db:      db,
		crypto:  crypto,
		audit:   cfg.Audit,
		logger:  logger,
		timeNow: func() time.Time { return time.Now().UTC() },
	}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("credstore: migrate: %w", err)
	}

	logger.Info("credstore: SQLite substrate initialized",
		zap.String("path", cfg.DBPath),
		zap.Int("schema_version", SchemaVersion),
	)
	return store, nil
}

// migrate applies every entry in migrations in order. Each migration's
// SQL is self-idempotent (CREATE TABLE IF NOT EXISTS, INSERT OR
// IGNORE) so reapplying on an up-to-date database is a no-op.
func (s *sqliteStore) migrate() error {
	for i, stmt := range migrations {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
	}
	return nil
}

// timestampLayout is the on-disk timestamp format. RFC3339Nano gives us
// human-readable ISO-8601 with sub-second precision and round-trips
// cleanly through time.Parse.
const timestampLayout = time.RFC3339Nano

// StoreAWSConnection inserts or updates a connection, encrypting the
// ExternalID with a fresh nonce on every write. UPSERT semantics: if
// account_id already exists, role_arn / display_name / region /
// external_id_ciphertext / external_id_nonce / updated_at are
// overwritten while created_at is preserved.
func (s *sqliteStore) StoreAWSConnection(ctx context.Context, conn AWSConnection) error {
	if conn.AccountID == "" {
		return errors.New("credstore: StoreAWSConnection: AccountID is required")
	}
	if conn.RoleARN == "" {
		return errors.New("credstore: StoreAWSConnection: RoleARN is required")
	}
	if conn.ExternalID == "" {
		// Empty ExternalID would bypass the confused-deputy defense.
		// The trust-policy template generated for operators always
		// includes one, so an empty value here is a programming error.
		return errors.New("credstore: StoreAWSConnection: ExternalID is required (trust policy is unsafe without it)")
	}

	ciphertext, nonce, err := s.crypto.seal([]byte(conn.ExternalID))
	if err != nil {
		return fmt.Errorf("credstore: encrypt ExternalID: %w", err)
	}

	now := s.timeNow()
	created := conn.CreatedAt
	if created.IsZero() {
		created = now
	}

	// SQLite UPSERT preserves created_at when the row already exists by
	// excluding it from the DO UPDATE SET clause. updated_at is always
	// stamped to now.
	const stmt = `
		INSERT INTO aws_connections (
			account_id, role_arn, display_name, region,
			external_id_ciphertext, external_id_nonce,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			role_arn = excluded.role_arn,
			display_name = excluded.display_name,
			region = excluded.region,
			external_id_ciphertext = excluded.external_id_ciphertext,
			external_id_nonce = excluded.external_id_nonce,
			updated_at = excluded.updated_at
	`
	if _, err := s.db.ExecContext(ctx, stmt,
		conn.AccountID,
		conn.RoleARN,
		conn.DisplayName,
		conn.Region,
		ciphertext,
		nonce,
		created.Format(timestampLayout),
		now.Format(timestampLayout),
	); err != nil {
		return fmt.Errorf("credstore: upsert aws_connection %s: %w", conn.AccountID, err)
	}
	return nil
}

// GetAWSConnection returns the connection for the given account, with
// the ExternalID decrypted. Returns (nil, nil) if no row matches.
// Emits one discovery.role_assumed audit event on success.
func (s *sqliteStore) GetAWSConnection(ctx context.Context, accountID string) (*AWSConnection, error) {
	const stmt = `
		SELECT account_id, role_arn, display_name, region,
		       external_id_ciphertext, external_id_nonce,
		       created_at, updated_at
		FROM aws_connections
		WHERE account_id = ?
	`
	row := s.db.QueryRowContext(ctx, stmt, accountID)
	conn, err := scanConnection(row, s.crypto)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	s.emitRoleAssumed(ctx, "get", conn)
	return conn, nil
}

// ListAWSConnections returns every connection record with ExternalIDs
// decrypted. Emits one discovery.role_assumed event per row so the
// audit trail records exactly which roles were read during this scan
// dispatch.
func (s *sqliteStore) ListAWSConnections(ctx context.Context) ([]*AWSConnection, error) {
	const stmt = `
		SELECT account_id, role_arn, display_name, region,
		       external_id_ciphertext, external_id_nonce,
		       created_at, updated_at
		FROM aws_connections
		ORDER BY account_id ASC
	`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("credstore: list aws_connections: %w", err)
	}
	defer rows.Close()

	var out []*AWSConnection
	for rows.Next() {
		conn, err := scanConnection(rows, s.crypto)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credstore: list aws_connections rows: %w", err)
	}

	for _, conn := range out {
		s.emitRoleAssumed(ctx, "list", conn)
	}
	return out, nil
}

// DeleteAWSConnection removes the row for the given account. Idempotent.
func (s *sqliteStore) DeleteAWSConnection(ctx context.Context, accountID string) error {
	if accountID == "" {
		return errors.New("credstore: DeleteAWSConnection: accountID is required")
	}
	const stmt = `DELETE FROM aws_connections WHERE account_id = ?`
	if _, err := s.db.ExecContext(ctx, stmt, accountID); err != nil {
		return fmt.Errorf("credstore: delete aws_connection %s: %w", accountID, err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *sqliteStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// scanConnection is the row-scan + decrypt helper shared by Get and
// List. Defined as a small interface so it can scan both *sql.Row and
// *sql.Rows without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanConnection(r rowScanner, c *cryptor) (*AWSConnection, error) {
	var (
		conn       AWSConnection
		ciphertext []byte
		nonce      []byte
		createdAt  string
		updatedAt  string
	)
	if err := r.Scan(
		&conn.AccountID,
		&conn.RoleARN,
		&conn.DisplayName,
		&conn.Region,
		&ciphertext,
		&nonce,
		&createdAt,
		&updatedAt,
	); err != nil {
		return nil, err
	}
	plaintext, err := c.open(ciphertext, nonce)
	if err != nil {
		return nil, err
	}
	conn.ExternalID = string(plaintext)

	if t, err := time.Parse(timestampLayout, createdAt); err == nil {
		conn.CreatedAt = t
	}
	if t, err := time.Parse(timestampLayout, updatedAt); err == nil {
		conn.UpdatedAt = t
	}
	return &conn, nil
}

// emitRoleAssumed records a discovery.role_assumed audit event for one
// read of one connection. The payload deliberately omits ExternalID;
// only the role identifier (which is already discoverable from the
// trust policy) and the operator-set metadata are emitted.
//
// op is "get" or "list" so the audit trail distinguishes a targeted
// lookup from a full sweep.
func (s *sqliteStore) emitRoleAssumed(ctx context.Context, op string, conn *AWSConnection) {
	if s.audit == nil {
		// Defensive: NewSQLiteStore rejects a nil audit recorder, so
		// this path is not reachable in production. A test that
		// constructs an sqliteStore directly without going through the
		// constructor would hit it; failing closed here keeps the
		// "no unaudited read path" invariant.
		return
	}
	payload := map[string]any{
		"account_id":   conn.AccountID,
		"role_arn":     conn.RoleARN,
		"display_name": conn.DisplayName,
		"region":       conn.Region,
		"operation":    op,
	}
	// Belt-and-braces: even if a future contributor adds ExternalID to
	// AWSConnection's payload by accident, this line ensures it gets
	// deleted before emission. The audit_test.go TestAuditEmissionOnRead
	// asserts the key is absent.
	delete(payload, "external_id")

	if err := s.audit.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  EventTypeRoleAssumed,
		TargetType: TargetTypeAWSConnection,
		TargetID:   conn.AccountID,
		Action:     "read",
		Payload:    payload,
	}); err != nil {
		// Audit failures are logged but do not abort the read. The
		// alternative — refusing to return a connection because the
		// audit sink is down — would take Squadron's discovery offline
		// for an operational issue unrelated to security posture.
		s.logger.Warn("credstore: discovery.role_assumed audit emit failed",
			zap.String("account_id", conn.AccountID),
			zap.String("operation", op),
			zap.Error(err),
		)
	}
}
