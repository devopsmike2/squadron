// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ociconnstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// sqliteStore is the SQLite-backed Store implementation. It owns the
// database handle it was constructed with so the substrate's
// lifecycle is independent — operators can wipe ociconnstore data
// without touching credstore, iacconnstore, gcpconnstore,
// azureconnstore, or the application store.
type sqliteStore struct {
	db      *sql.DB
	logger  *zap.Logger
	timeNow func() time.Time // injectable so tests can pin timestamps
	newUUID func() string    // injectable so tests can pin IDs
}

// Config configures the SQLite-backed helper constructor
// NewSQLiteStore. Callers that already manage their own *sql.DB can
// skip this and call NewStore directly.
//
//   - DBPath is the SQLite database file path. ":memory:" is
//     supported for tests.
//   - Logger is optional; defaults to zap.NewNop() when nil. The
//     logger is intentionally only used for non-sensitive operational
//     lines (open / migration / close). SealedPrivateKey bytes are
//     NEVER logged.
type Config struct {
	DBPath string
	Logger *zap.Logger
}

// NewSQLiteStore opens (or creates) the substrate's SQLite database,
// runs migrations, and returns a Store ready to use. Mirrors
// azureconnstore.NewSQLiteStore exactly so the cmd/all-in-one wiring
// reads parallel.
func NewSQLiteStore(cfg Config) (Store, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("ociconnstore: Config.DBPath is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("ociconnstore: open sqlite at %q: %w", cfg.DBPath, err)
	}
	// Match azureconnstore's pool tuning so behavior is predictable
	// on shared hosts. Volume here is even lower than iacconnstore
	// (one row per connected tenancy, not per connected repo).
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ociconnstore: enable foreign keys: %w", err)
	}

	store, err := NewStore(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if s, ok := store.(*sqliteStore); ok {
		s.logger = logger
	}

	logger.Info("ociconnstore: SQLite substrate initialized",
		zap.String("path", cfg.DBPath),
		zap.Int("schema_version", SchemaVersion),
	)
	return store, nil
}

// NewStore is the substrate's primary constructor. It accepts a
// pre-opened *sql.DB, runs migrations, and returns a Store ready to
// use. The OSS NewSQLiteStore helper is a convenience that wraps
// this constructor with the standard SQLite settings.
func NewStore(ctx context.Context, db *sql.DB) (Store, error) {
	if db == nil {
		return nil, errors.New("ociconnstore: NewStore: db is required")
	}
	store := &sqliteStore{
		db:      db,
		logger:  zap.NewNop(),
		timeNow: func() time.Time { return time.Now().UTC() },
		newUUID: func() string { return uuid.NewString() },
	}
	if err := store.migrate(ctx); err != nil {
		return nil, fmt.Errorf("ociconnstore: migrate: %w", err)
	}
	return store, nil
}

// migrate applies every entry in migrations in order. Each migration
// is self-idempotent (CREATE TABLE IF NOT EXISTS, INSERT OR IGNORE)
// so reapplying on an up-to-date database is a no-op. The runner
// tolerates the "duplicate column name" error on re-run so future
// ALTER TABLE migrations behave the same way azureconnstore's do.
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

// isDuplicateColumnErr reports whether err is the SQLite-driver
// surface of an ALTER TABLE ADD COLUMN against a column that already
// exists. SQLite phrases it as "duplicate column name: <col>".
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// timestampLayout is the on-disk timestamp format. RFC3339Nano gives
// human-readable ISO-8601 with sub-second precision and round-trips
// cleanly through time.Parse. Matches azureconnstore.
const timestampLayout = time.RFC3339Nano

// Create inserts a new connection row. The ID is generated here
// (callers leave it empty on the input); CreatedAt and UpdatedAt are
// stamped to now and written back to the passed-in struct so the
// caller can return them in the API response.
func (s *sqliteStore) Create(ctx context.Context, conn *OCIConnection) error {
	if conn == nil {
		return errors.New("ociconnstore: Create: conn is required")
	}
	if conn.DisplayName == "" {
		return errors.New("ociconnstore: Create: DisplayName is required")
	}
	if conn.TenancyOCID == "" {
		return errors.New("ociconnstore: Create: TenancyOCID is required")
	}
	if conn.UserOCID == "" {
		return errors.New("ociconnstore: Create: UserOCID is required")
	}
	if conn.Fingerprint == "" {
		return errors.New("ociconnstore: Create: Fingerprint is required")
	}
	if len(conn.SealedPrivateKey) == 0 {
		return errors.New("ociconnstore: Create: SealedPrivateKey is required (callers must seal via credstore.SealOCIPrivateKey)")
	}
	// OCI is regional — Region is REQUIRED unlike AWS/GCP/Azure
	// where empty Region means "scan all". The wizard (chunk 3) and
	// the schema NOT NULL are the other two lines of defense; this
	// is the Go-side check.
	if conn.Region == "" {
		return errors.New("ociconnstore: Create: Region is required (OCI's API endpoints are regional)")
	}

	now := s.timeNow()
	conn.ID = s.newUUID()
	conn.CreatedAt = now
	conn.UpdatedAt = now
	// Default the discovery feedback-loop flag to true (opt-in) on
	// Create, mirroring iacconnstore's v0.89.28, gcpconnstore's
	// v0.89.46, and azureconnstore's v0.89.51 posture. Callers may
	// override before passing the struct in; callers that leave the
	// zero value (false) get the design default by way of this stamp.
	if !conn.LearnFromAcceptedRecommendations {
		conn.LearnFromAcceptedRecommendations = true
	}
	learnInt := 1
	if !conn.LearnFromAcceptedRecommendations {
		learnInt = 0
	}

	// ADR 0013 §D6-b: default the Squadron owner tenant to the OSS
	// single-tenant sentinel when the caller left it empty. This is the
	// squadron_tenant_id column — distinct from the OCI tenancy_ocid.
	// The create handler stamps identity.TenantFromContext(ctx) onto the
	// struct before Create; an unstamped struct (direct test
	// construction, background path) still lands a valid "default" row.
	ownerTenantID := conn.OwnerTenantID
	if ownerTenantID == "" {
		ownerTenantID = "default"
	}
	conn.OwnerTenantID = ownerTenantID

	const stmt = `
		INSERT INTO oci_connections (
			id, display_name, tenancy_ocid, user_ocid, fingerprint,
			sealed_private_key, region,
			learn_from_accepted_recommendations,
			squadron_tenant_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if _, err := s.db.ExecContext(ctx, stmt,
		conn.ID,
		conn.DisplayName,
		conn.TenancyOCID,
		conn.UserOCID,
		conn.Fingerprint,
		conn.SealedPrivateKey,
		conn.Region,
		learnInt,
		ownerTenantID,
		now.Format(timestampLayout),
		now.Format(timestampLayout),
	); err != nil {
		return fmt.Errorf("ociconnstore: insert oci_connection: %w", err)
	}
	return nil
}

// Get returns the connection row for the given ID. Returns
// ErrConnectionNotFound when no row matches.
func (s *sqliteStore) Get(ctx context.Context, id string) (*OCIConnection, error) {
	if id == "" {
		return nil, errors.New("ociconnstore: Get: id is required")
	}
	const stmt = `
		SELECT id, display_name, tenancy_ocid, user_ocid, fingerprint,
		       sealed_private_key, region,
		       learn_from_accepted_recommendations,
		       squadron_tenant_id,
		       created_at, updated_at
		FROM oci_connections
		WHERE id = ?
	`
	row := s.db.QueryRowContext(ctx, stmt, id)
	conn, err := scanConnection(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConnectionNotFound
		}
		return nil, err
	}
	return conn, nil
}

// List returns every connection row, ordered by created_at ascending.
func (s *sqliteStore) List(ctx context.Context) ([]*OCIConnection, error) {
	const stmt = `
		SELECT id, display_name, tenancy_ocid, user_ocid, fingerprint,
		       sealed_private_key, region,
		       learn_from_accepted_recommendations,
		       squadron_tenant_id,
		       created_at, updated_at
		FROM oci_connections
		ORDER BY created_at ASC, id ASC
	`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("ociconnstore: list oci_connections: %w", err)
	}
	defer rows.Close()

	var out []*OCIConnection
	for rows.Next() {
		conn, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ociconnstore: list oci_connections rows: %w", err)
	}
	return out, nil
}

// Update replaces the mutable fields on the row and stamps UpdatedAt.
// A nil/empty SealedPrivateKey leaves the existing column in place —
// callers that want to rotate must pass a freshly sealed blob.
func (s *sqliteStore) Update(ctx context.Context, conn *OCIConnection) error {
	if conn == nil {
		return errors.New("ociconnstore: Update: conn is required")
	}
	if conn.ID == "" {
		return errors.New("ociconnstore: Update: ID is required")
	}
	if conn.DisplayName == "" {
		return errors.New("ociconnstore: Update: DisplayName is required")
	}
	if conn.TenancyOCID == "" {
		return errors.New("ociconnstore: Update: TenancyOCID is required")
	}
	if conn.UserOCID == "" {
		return errors.New("ociconnstore: Update: UserOCID is required")
	}
	if conn.Fingerprint == "" {
		return errors.New("ociconnstore: Update: Fingerprint is required")
	}
	if conn.Region == "" {
		return errors.New("ociconnstore: Update: Region is required (OCI's API endpoints are regional)")
	}
	learnInt := 0
	if conn.LearnFromAcceptedRecommendations {
		learnInt = 1
	}
	now := s.timeNow()

	var (
		res sql.Result
		err error
	)
	if len(conn.SealedPrivateKey) == 0 {
		const stmt = `
			UPDATE oci_connections
			SET display_name = ?, tenancy_ocid = ?, user_ocid = ?,
			    fingerprint = ?, region = ?,
			    learn_from_accepted_recommendations = ?,
			    updated_at = ?
			WHERE id = ?
		`
		res, err = s.db.ExecContext(ctx, stmt,
			conn.DisplayName,
			conn.TenancyOCID,
			conn.UserOCID,
			conn.Fingerprint,
			conn.Region,
			learnInt,
			now.Format(timestampLayout),
			conn.ID,
		)
	} else {
		const stmt = `
			UPDATE oci_connections
			SET display_name = ?, tenancy_ocid = ?, user_ocid = ?,
			    fingerprint = ?, sealed_private_key = ?, region = ?,
			    learn_from_accepted_recommendations = ?,
			    updated_at = ?
			WHERE id = ?
		`
		res, err = s.db.ExecContext(ctx, stmt,
			conn.DisplayName,
			conn.TenancyOCID,
			conn.UserOCID,
			conn.Fingerprint,
			conn.SealedPrivateKey,
			conn.Region,
			learnInt,
			now.Format(timestampLayout),
			conn.ID,
		)
	}
	if err != nil {
		return fmt.Errorf("ociconnstore: update oci_connection %s: %w", conn.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ociconnstore: rows affected for %s: %w", conn.ID, err)
	}
	if affected == 0 {
		return ErrConnectionNotFound
	}
	conn.UpdatedAt = now
	return nil
}

// Delete removes the row for the given ID. Idempotent.
func (s *sqliteStore) Delete(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("ociconnstore: Delete: id is required")
	}
	const stmt = `DELETE FROM oci_connections WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, stmt, id); err != nil {
		return fmt.Errorf("ociconnstore: delete oci_connection %s: %w", id, err)
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

// rowScanner is the small interface that lets scanConnection scan
// both *sql.Row and *sql.Rows without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanConnection is the row-scan helper shared by Get and List.
func scanConnection(r rowScanner) (*OCIConnection, error) {
	var (
		conn              OCIConnection
		learnFromAccepted int
		ownerTenantID     string
		createdAt         string
		updatedAt         string
	)
	if err := r.Scan(
		&conn.ID,
		&conn.DisplayName,
		&conn.TenancyOCID,
		&conn.UserOCID,
		&conn.Fingerprint,
		&conn.SealedPrivateKey,
		&conn.Region,
		&learnFromAccepted,
		&ownerTenantID,
		&createdAt,
		&updatedAt,
	); err != nil {
		return nil, err
	}
	conn.LearnFromAcceptedRecommendations = learnFromAccepted != 0
	// ADR 0013 §D6-b: NOT NULL DEFAULT 'default' guarantees a non-empty
	// value on disk, but guard the empty case so a hand-edited row still
	// reads back the OSS single-tenant sentinel. This is the Squadron
	// owner tenant, distinct from the OCI conn.TenancyOCID above.
	if ownerTenantID == "" {
		ownerTenantID = "default"
	}
	conn.OwnerTenantID = ownerTenantID
	if t, err := time.Parse(timestampLayout, createdAt); err == nil {
		conn.CreatedAt = t
	}
	if t, err := time.Parse(timestampLayout, updatedAt); err == nil {
		conn.UpdatedAt = t
	}
	return &conn, nil
}
