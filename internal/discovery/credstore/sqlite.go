// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// sqliteStore is the SQLite-backed Store implementation. It owns the
// database handle it was constructed with (separate from the
// application store) so the substrate's lifecycle is independent —
// operators can wipe credstore data without touching the rest of
// Squadron, and credstore migrations don't entangle with
// application-store migrations.
type sqliteStore struct {
	db      *sql.DB
	backend SecretsBackend
	audit   AuditRecorder
	logger  *zap.Logger
	timeNow func() time.Time // injectable so tests can pin timestamps
}

// Config configures the SQLite-backed helper constructor NewSQLiteStore.
// Callers that already manage their own *sql.DB and SecretsBackend can
// skip this and call NewStore directly.
//
//   - DBPath is the SQLite database file path. ":memory:" is supported
//     for tests.
//   - Audit is the audit recorder that receives the per-read events.
//     Required — the substrate refuses to start without one because
//     every read MUST be audited.
//   - Logger is optional; defaults to zap.NewNop() when nil. The logger
//     is intentionally only used for non-sensitive operational lines
//     (open / migration / close). Credentials are NEVER logged.
type Config struct {
	DBPath string
	Audit  AuditRecorder
	Logger *zap.Logger
}

// NewSQLiteStore opens (or creates) the substrate's SQLite database,
// constructs a SQLiteSecretsBackend from SQUADRON_SECRETS_KEY, runs
// migrations, and returns a Store ready to use.
//
// Construction fails loud on any of:
//
//   - SQUADRON_SECRETS_KEY missing or wrong length
//   - audit recorder nil
//   - database open / migration error
//
// No fallback to a default key, a zero key, or plaintext exists. The
// helper is a thin wrapper around NewStore — callers wiring a
// non-SQLite SecretsBackend (e.g. Vault from the Compliance Pack) call
// NewStore directly with their own backend.
func NewSQLiteStore(cfg Config) (Store, error) {
	if cfg.Audit == nil {
		return nil, errors.New("credstore: Config.Audit is required; the substrate has no unaudited read path")
	}
	if cfg.DBPath == "" {
		return nil, errors.New("credstore: Config.DBPath is required")
	}

	key, err := LoadKeyFromEnv()
	if err != nil {
		return nil, err
	}
	backend := NewSQLiteSecretsBackend(key)

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

	store, err := NewStore(context.Background(), db, backend, cfg.Audit)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	// NewStore returns *sqliteStore but typed as Store; set the
	// optional fields the helper exposes (logger). We know the
	// concrete type because NewStore is in this package.
	if s, ok := store.(*sqliteStore); ok {
		s.logger = logger
	}

	logger.Info("credstore: SQLite substrate initialized",
		zap.String("path", cfg.DBPath),
		zap.Int("schema_version", SchemaVersion),
	)
	return store, nil
}

// NewStore is the substrate's primary constructor. It accepts a
// pre-opened *sql.DB, a SecretsBackend, and an AuditRecorder, runs
// migrations against the database, and returns a Store ready to use.
//
// This is what the Compliance Pack's Vault-backed deployment calls —
// the Pack manages its own *sql.DB lifecycle and constructs a
// Vault-backed SecretsBackend. The OSS NewSQLiteStore helper is a
// convenience that wraps this constructor with the standard SQLite +
// env-keyed backend.
func NewStore(ctx context.Context, db *sql.DB, backend SecretsBackend, audit AuditRecorder) (Store, error) {
	if db == nil {
		return nil, errors.New("credstore: NewStore: db is required")
	}
	if backend == nil {
		return nil, errors.New("credstore: NewStore: backend is required")
	}
	if audit == nil {
		return nil, errors.New("credstore: NewStore: audit is required; the substrate has no unaudited read path")
	}

	store := &sqliteStore{
		db:      db,
		backend: backend,
		audit:   audit,
		logger:  zap.NewNop(),
		timeNow: func() time.Time { return time.Now().UTC() },
	}
	if err := store.migrate(ctx); err != nil {
		return nil, fmt.Errorf("credstore: migrate: %w", err)
	}
	return store, nil
}

// migrate applies every entry in migrations in order. Each migration's
// SQL is self-idempotent (CREATE TABLE IF NOT EXISTS, INSERT OR
// IGNORE, DROP TABLE IF EXISTS) so reapplying on an up-to-date
// database is a no-op — with one exception: ALTER TABLE ... ADD COLUMN
// cannot be expressed idempotently in SQLite. The runner tolerates the
// "duplicate column name" error on re-run so the ADR 0013 §D6-b
// migration0003TenantID can land on existing databases without
// breaking startup on the second boot (mirroring iacconnstore's
// migrate). Prior to D6-b credstore had no ALTER migration and so no
// guard; the guard is added here alongside the first ALTER.
func (s *sqliteStore) migrate(ctx context.Context) error {
	for i, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			if isDuplicateColumnErr(err) {
				continue
			}
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
	}
	return nil
}

// isDuplicateColumnErr reports whether err is the SQLite-driver surface
// of an ALTER TABLE ADD COLUMN against a column that already exists.
// SQLite phrases it as "duplicate column name: <col>". Mirrors
// iacconnstore.isDuplicateColumnErr so a re-run of the ADR 0013 §D6-b
// ADD COLUMN migration against an already-migrated database is a no-op
// rather than a startup error.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// timestampLayout is the on-disk timestamp format. RFC3339Nano gives us
// human-readable ISO-8601 with sub-second precision and round-trips
// cleanly through time.Parse.
const timestampLayout = time.RFC3339Nano

// StoreConnection inserts or updates a connection row. The Credentials
// field is expected to already be encrypted (via the per-provider
// Marshal helper, e.g. MarshalAWSCredentials). UPSERT semantics: if
// account_id already exists, every column except created_at is
// overwritten; created_at is preserved.
func (s *sqliteStore) StoreConnection(ctx context.Context, conn CloudConnection) error {
	if conn.AccountID == "" {
		return errors.New("credstore: StoreConnection: AccountID is required")
	}
	if conn.Provider == "" {
		return errors.New("credstore: StoreConnection: Provider is required")
	}
	if conn.ConnectionType == "" {
		return errors.New("credstore: StoreConnection: ConnectionType is required")
	}
	if len(conn.Credentials) == 0 {
		return errors.New("credstore: StoreConnection: Credentials ciphertext is required (callers must encrypt via the per-provider helper)")
	}
	if len(conn.CredentialsNonce) == 0 {
		return errors.New("credstore: StoreConnection: CredentialsNonce is required")
	}

	// Regions is serialized as a JSON array TEXT column. A nil slice
	// becomes "[]" so the column is NOT NULL and readers don't need
	// to special-case empty.
	regions := conn.Regions
	if regions == nil {
		regions = []string{}
	}
	regionsJSON, err := json.Marshal(regions)
	if err != nil {
		return fmt.Errorf("credstore: marshal Regions: %w", err)
	}

	now := s.timeNow()
	created := conn.CreatedAt
	if created.IsZero() {
		created = now
	}

	// ADR 0013 §D6-b: default the owner tenant to the OSS single-tenant
	// sentinel when the caller left it empty. The AWS save handler
	// stamps identity.TenantFromContext(ctx) onto the struct before
	// StoreConnection; an unstamped struct (direct test construction,
	// background path) still lands a valid "default" row.
	tenantID := conn.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	conn.TenantID = tenantID

	// SQLite UPSERT preserves created_at when the row already exists by
	// excluding it from the DO UPDATE SET clause. updated_at is always
	// stamped to now. tenant_id is likewise EXCLUDED from DO UPDATE SET:
	// ownership is immutable (ADR 0013 §D6-b), so a re-save of an
	// existing connection preserves the tenant that first created it
	// rather than re-stamping it from the re-saver's context.
	const stmt = `
		INSERT INTO cloud_connections (
			account_id, provider, connection_type, display_name, regions,
			credentials_ciphertext, credentials_nonce,
			tenant_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			provider = excluded.provider,
			connection_type = excluded.connection_type,
			display_name = excluded.display_name,
			regions = excluded.regions,
			credentials_ciphertext = excluded.credentials_ciphertext,
			credentials_nonce = excluded.credentials_nonce,
			updated_at = excluded.updated_at
	`
	if _, err := s.db.ExecContext(ctx, stmt,
		conn.AccountID,
		string(conn.Provider),
		string(conn.ConnectionType),
		conn.DisplayName,
		string(regionsJSON),
		conn.Credentials,
		conn.CredentialsNonce,
		tenantID,
		created.Format(timestampLayout),
		now.Format(timestampLayout),
	); err != nil {
		return fmt.Errorf("credstore: upsert cloud_connection %s: %w", conn.AccountID, err)
	}
	return nil
}

// GetConnection returns the connection row for the given account.
// Credentials and CredentialsNonce hold the on-disk ciphertext; the
// caller decrypts via the per-provider Unmarshal helper. Returns
// (nil, nil) if no row matches. Emits one
// discovery.<provider>.connection_read event on success.
func (s *sqliteStore) GetConnection(ctx context.Context, accountID string) (*CloudConnection, error) {
	const stmt = `
		SELECT account_id, provider, connection_type, display_name, regions,
		       credentials_ciphertext, credentials_nonce,
		       tenant_id,
		       created_at, updated_at
		FROM cloud_connections
		WHERE account_id = ?
	`
	row := s.db.QueryRowContext(ctx, stmt, accountID)
	conn, err := scanConnection(row, s.backend)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	s.emitConnectionRead(ctx, "get", conn)
	return conn, nil
}

// ListConnections returns every connection row that matches filter.
// An empty filter returns all rows. Emits one
// discovery.<provider>.connection_read event per row so the audit
// trail records exactly which connections were read during a scan
// dispatch.
func (s *sqliteStore) ListConnections(ctx context.Context, filter ListFilter) ([]*CloudConnection, error) {
	const base = `
		SELECT account_id, provider, connection_type, display_name, regions,
		       credentials_ciphertext, credentials_nonce,
		       tenant_id,
		       created_at, updated_at
		FROM cloud_connections
	`
	var (
		rows *sql.Rows
		err  error
	)
	if filter.Provider == "" {
		rows, err = s.db.QueryContext(ctx, base+` ORDER BY account_id ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			base+` WHERE provider = ? ORDER BY account_id ASC`,
			string(filter.Provider))
	}
	if err != nil {
		return nil, fmt.Errorf("credstore: list cloud_connections: %w", err)
	}
	defer rows.Close()

	var out []*CloudConnection
	for rows.Next() {
		conn, err := scanConnection(rows, s.backend)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credstore: list cloud_connections rows: %w", err)
	}

	for _, conn := range out {
		s.emitConnectionRead(ctx, "list", conn)
	}
	return out, nil
}

// DeleteConnection removes the row for the given account. Idempotent.
func (s *sqliteStore) DeleteConnection(ctx context.Context, accountID string) error {
	if accountID == "" {
		return errors.New("credstore: DeleteConnection: accountID is required")
	}
	const stmt = `DELETE FROM cloud_connections WHERE account_id = ?`
	if _, err := s.db.ExecContext(ctx, stmt, accountID); err != nil {
		return fmt.Errorf("credstore: delete cloud_connection %s: %w", accountID, err)
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

// scanConnection is the row-scan helper shared by Get and List.
// Defined against a small interface so it can scan both *sql.Row and
// *sql.Rows without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanConnection(r rowScanner, _ SecretsBackend) (*CloudConnection, error) {
	var (
		conn        CloudConnection
		provider    string
		connType    string
		regionsJSON string
		ciphertext  []byte
		nonce       []byte
		tenantID    string
		createdAt   string
		updatedAt   string
	)
	if err := r.Scan(
		&conn.AccountID,
		&provider,
		&connType,
		&conn.DisplayName,
		&regionsJSON,
		&ciphertext,
		&nonce,
		&tenantID,
		&createdAt,
		&updatedAt,
	); err != nil {
		return nil, err
	}
	conn.Provider = Provider(provider)
	conn.ConnectionType = ConnectionType(connType)

	// ADR 0013 §D6-b: NOT NULL DEFAULT 'default' guarantees a non-empty
	// value on disk, but guard the empty case so a hand-edited row still
	// reads back the OSS single-tenant sentinel.
	if tenantID == "" {
		tenantID = "default"
	}
	conn.TenantID = tenantID

	if err := json.Unmarshal([]byte(regionsJSON), &conn.Regions); err != nil {
		return nil, fmt.Errorf("credstore: unmarshal regions JSON for %s: %w", conn.AccountID, err)
	}

	// Credentials remain encrypted at the substrate boundary. The
	// caller decrypts via the per-provider Unmarshal helper. The
	// SecretsBackend parameter is kept on the signature so a future
	// "decrypt at scan time" extension is additive; today the
	// substrate is intentionally a transport-only layer for the blob.
	conn.Credentials = ciphertext
	conn.CredentialsNonce = nonce

	if t, err := time.Parse(timestampLayout, createdAt); err == nil {
		conn.CreatedAt = t
	}
	if t, err := time.Parse(timestampLayout, updatedAt); err == nil {
		conn.UpdatedAt = t
	}
	return &conn, nil
}

// emitConnectionRead records a discovery.<provider>.connection_read
// audit event for one read of one connection. The payload deliberately
// omits the credentials bytes; only the connection metadata is
// emitted.
//
// op is "get" or "list" so the audit trail distinguishes a targeted
// lookup from a full sweep.
func (s *sqliteStore) emitConnectionRead(ctx context.Context, op string, conn *CloudConnection) {
	if s.audit == nil {
		// Defensive: NewStore rejects a nil audit recorder, so this
		// path is not reachable in production. A test that constructs
		// an sqliteStore directly without going through the
		// constructor would hit it; failing closed here keeps the
		// "no unaudited read path" invariant.
		return
	}
	payload := map[string]any{
		"account_id":      conn.AccountID,
		"provider":        string(conn.Provider),
		"connection_type": string(conn.ConnectionType),
		"display_name":    conn.DisplayName,
		"regions":         conn.Regions,
		"operation":       op,
	}
	// Belt-and-braces: ensure no future contributor accidentally adds a
	// credentials field to the payload. Both keys are explicitly
	// scrubbed before emission.
	delete(payload, "credentials")
	delete(payload, "credentials_ciphertext")

	if err := s.audit.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  FormatConnectionReadEvent(conn.Provider),
		TargetType: TargetTypeCloudConnection,
		TargetID:   conn.AccountID,
		Action:     "read",
		Payload:    payload,
	}); err != nil {
		// Audit failures are logged but do not abort the read. The
		// alternative — refusing to return a connection because the
		// audit sink is down — would take Squadron's discovery offline
		// for an operational issue unrelated to security posture.
		s.logger.Warn("credstore: connection_read audit emit failed",
			zap.String("account_id", conn.AccountID),
			zap.String("provider", string(conn.Provider)),
			zap.String("operation", op),
			zap.Error(err),
		)
	}
}
