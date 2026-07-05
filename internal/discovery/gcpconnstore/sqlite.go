// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcpconnstore

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
// lifecycle is independent — operators can wipe gcpconnstore data
// without touching credstore, iacconnstore, or the application
// store.
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
//     lines (open / migration / close). SealedSA bytes are NEVER
//     logged.
type Config struct {
	DBPath string
	Logger *zap.Logger
}

// NewSQLiteStore opens (or creates) the substrate's SQLite database,
// runs migrations, and returns a Store ready to use. Mirrors
// iacconnstore.NewSQLiteStore exactly so the cmd/all-in-one wiring
// reads parallel.
func NewSQLiteStore(cfg Config) (Store, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("gcpconnstore: Config.DBPath is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("gcpconnstore: open sqlite at %q: %w", cfg.DBPath, err)
	}
	// Match iacconnstore's pool tuning so behavior is predictable on
	// shared hosts. Volume here is even lower than iacconnstore (one
	// row per connected project, not per connected repo).
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("gcpconnstore: enable foreign keys: %w", err)
	}

	store, err := NewStore(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if s, ok := store.(*sqliteStore); ok {
		s.logger = logger
	}

	logger.Info("gcpconnstore: SQLite substrate initialized",
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
		return nil, errors.New("gcpconnstore: NewStore: db is required")
	}
	store := &sqliteStore{
		db:      db,
		logger:  zap.NewNop(),
		timeNow: func() time.Time { return time.Now().UTC() },
		newUUID: func() string { return uuid.NewString() },
	}
	if err := store.migrate(ctx); err != nil {
		return nil, fmt.Errorf("gcpconnstore: migrate: %w", err)
	}
	return store, nil
}

// migrate applies every entry in migrations in order. Each migration
// is self-idempotent (CREATE TABLE IF NOT EXISTS, INSERT OR IGNORE)
// so reapplying on an up-to-date database is a no-op. The runner
// tolerates the "duplicate column name" error on re-run so future
// ALTER TABLE migrations behave the same way iacconnstore's do.
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
// cleanly through time.Parse. Matches iacconnstore.
const timestampLayout = time.RFC3339Nano

// Create inserts a new connection row. The ID is generated here
// (callers leave it empty on the input); CreatedAt and UpdatedAt are
// stamped to now and written back to the passed-in struct so the
// caller can return them in the API response.
func (s *sqliteStore) Create(ctx context.Context, conn *GCPConnection) error {
	if conn == nil {
		return errors.New("gcpconnstore: Create: conn is required")
	}
	if conn.DisplayName == "" {
		return errors.New("gcpconnstore: Create: DisplayName is required")
	}
	if conn.ProjectID == "" {
		return errors.New("gcpconnstore: Create: ProjectID is required")
	}
	if len(conn.SealedSA) == 0 {
		return errors.New("gcpconnstore: Create: SealedSA is required (callers must seal via credstore.SealGCPServiceAccount)")
	}

	now := s.timeNow()
	conn.ID = s.newUUID()
	conn.CreatedAt = now
	conn.UpdatedAt = now
	// Default the discovery feedback-loop flag to true (opt-in) on
	// Create, mirroring iacconnstore's v0.89.28 posture. Callers may
	// override before passing the struct in; callers that leave the
	// zero value (false) get the design default by way of this stamp.
	if !conn.LearnFromAcceptedRecommendations {
		conn.LearnFromAcceptedRecommendations = true
	}
	learnInt := 1
	if !conn.LearnFromAcceptedRecommendations {
		learnInt = 0
	}

	// ADR 0013 §D6-b: default the owner tenant to the OSS single-tenant
	// sentinel when the caller left it empty. The create handler stamps
	// identity.TenantFromContext(ctx) onto the struct before Create; an
	// unstamped struct (direct test construction, background path) still
	// lands a valid "default" row rather than an empty tenant.
	tenantID := conn.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	conn.TenantID = tenantID

	const stmt = `
		INSERT INTO gcp_connections (
			id, display_name, project_id, sealed_sa, region,
			learn_from_accepted_recommendations,
			tenant_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if _, err := s.db.ExecContext(ctx, stmt,
		conn.ID,
		conn.DisplayName,
		conn.ProjectID,
		conn.SealedSA,
		nullableString(conn.Region),
		learnInt,
		tenantID,
		now.Format(timestampLayout),
		now.Format(timestampLayout),
	); err != nil {
		return fmt.Errorf("gcpconnstore: insert gcp_connection: %w", err)
	}
	return nil
}

// Get returns the connection row for the given ID. Returns
// ErrConnectionNotFound when no row matches.
func (s *sqliteStore) Get(ctx context.Context, id string) (*GCPConnection, error) {
	if id == "" {
		return nil, errors.New("gcpconnstore: Get: id is required")
	}
	const stmt = `
		SELECT id, display_name, project_id, sealed_sa, region,
		       learn_from_accepted_recommendations,
		       tenant_id,
		       created_at, updated_at
		FROM gcp_connections
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
func (s *sqliteStore) List(ctx context.Context) ([]*GCPConnection, error) {
	const stmt = `
		SELECT id, display_name, project_id, sealed_sa, region,
		       learn_from_accepted_recommendations,
		       tenant_id,
		       created_at, updated_at
		FROM gcp_connections
		ORDER BY created_at ASC, id ASC
	`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("gcpconnstore: list gcp_connections: %w", err)
	}
	defer rows.Close()

	var out []*GCPConnection
	for rows.Next() {
		conn, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gcpconnstore: list gcp_connections rows: %w", err)
	}
	return out, nil
}

// Update replaces the mutable fields on the row and stamps UpdatedAt.
// A nil/empty SealedSA leaves the existing column in place — callers
// that want to rotate must pass a freshly sealed blob.
func (s *sqliteStore) Update(ctx context.Context, conn *GCPConnection) error {
	if conn == nil {
		return errors.New("gcpconnstore: Update: conn is required")
	}
	if conn.ID == "" {
		return errors.New("gcpconnstore: Update: ID is required")
	}
	if conn.DisplayName == "" {
		return errors.New("gcpconnstore: Update: DisplayName is required")
	}
	if conn.ProjectID == "" {
		return errors.New("gcpconnstore: Update: ProjectID is required")
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
	if len(conn.SealedSA) == 0 {
		const stmt = `
			UPDATE gcp_connections
			SET display_name = ?, project_id = ?, region = ?,
			    learn_from_accepted_recommendations = ?,
			    updated_at = ?
			WHERE id = ?
		`
		res, err = s.db.ExecContext(ctx, stmt,
			conn.DisplayName,
			conn.ProjectID,
			nullableString(conn.Region),
			learnInt,
			now.Format(timestampLayout),
			conn.ID,
		)
	} else {
		const stmt = `
			UPDATE gcp_connections
			SET display_name = ?, project_id = ?, sealed_sa = ?,
			    region = ?, learn_from_accepted_recommendations = ?,
			    updated_at = ?
			WHERE id = ?
		`
		res, err = s.db.ExecContext(ctx, stmt,
			conn.DisplayName,
			conn.ProjectID,
			conn.SealedSA,
			nullableString(conn.Region),
			learnInt,
			now.Format(timestampLayout),
			conn.ID,
		)
	}
	if err != nil {
		return fmt.Errorf("gcpconnstore: update gcp_connection %s: %w", conn.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("gcpconnstore: rows affected for %s: %w", conn.ID, err)
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
		return errors.New("gcpconnstore: Delete: id is required")
	}
	const stmt = `DELETE FROM gcp_connections WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, stmt, id); err != nil {
		return fmt.Errorf("gcpconnstore: delete gcp_connection %s: %w", id, err)
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
func scanConnection(r rowScanner) (*GCPConnection, error) {
	var (
		conn              GCPConnection
		region            sql.NullString
		learnFromAccepted int
		tenantID          string
		createdAt         string
		updatedAt         string
	)
	if err := r.Scan(
		&conn.ID,
		&conn.DisplayName,
		&conn.ProjectID,
		&conn.SealedSA,
		&region,
		&learnFromAccepted,
		&tenantID,
		&createdAt,
		&updatedAt,
	); err != nil {
		return nil, err
	}
	conn.LearnFromAcceptedRecommendations = learnFromAccepted != 0
	// ADR 0013 §D6-b: NOT NULL DEFAULT 'default' guarantees a non-empty
	// value on disk, but guard the empty case so a hand-edited row still
	// reads back the OSS single-tenant sentinel.
	if tenantID == "" {
		tenantID = "default"
	}
	conn.TenantID = tenantID
	if region.Valid {
		conn.Region = region.String
	}
	if t, err := time.Parse(timestampLayout, createdAt); err == nil {
		conn.CreatedAt = t
	}
	if t, err := time.Parse(timestampLayout, updatedAt); err == nil {
		conn.UpdatedAt = t
	}
	return &conn, nil
}

// nullableString turns an empty string into a SQL NULL so the
// optional region column doesn't store "" — that way operators
// querying the DB directly can distinguish "scan all regions" from
// "the operator typed an empty string".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
