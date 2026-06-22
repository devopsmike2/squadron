// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// sqliteStore is the SQLite-backed Store implementation. It owns the
// database handle it was constructed with so the substrate's
// lifecycle is independent — operators can wipe iacconnstore data
// without touching credstore or the application store.
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
//     lines (open / migration / close). Credentials are NEVER logged.
type Config struct {
	DBPath string
	Logger *zap.Logger
}

// NewSQLiteStore opens (or creates) the substrate's SQLite database,
// runs migrations, and returns a Store ready to use. The helper is
// a thin wrapper around NewStore — callers wiring their own *sql.DB
// (for shared-pool deployments) call NewStore directly.
func NewSQLiteStore(cfg Config) (Store, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("iacconnstore: Config.DBPath is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("iacconnstore: open sqlite at %q: %w", cfg.DBPath, err)
	}
	// Match credstore's pool tuning so behavior is predictable on
	// shared hosts. Volume here is even lower than credstore (one
	// row per connected repo, not per connected account).
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("iacconnstore: enable foreign keys: %w", err)
	}

	store, err := NewStore(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if s, ok := store.(*sqliteStore); ok {
		s.logger = logger
	}

	logger.Info("iacconnstore: SQLite substrate initialized",
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
		return nil, errors.New("iacconnstore: NewStore: db is required")
	}
	store := &sqliteStore{
		db:      db,
		logger:  zap.NewNop(),
		timeNow: func() time.Time { return time.Now().UTC() },
		newUUID: func() string { return uuid.NewString() },
	}
	if err := store.migrate(ctx); err != nil {
		return nil, fmt.Errorf("iacconnstore: migrate: %w", err)
	}
	return store, nil
}

// migrate applies every entry in migrations in order. Each
// migration's SQL is self-idempotent (CREATE TABLE IF NOT EXISTS,
// INSERT OR IGNORE) so reapplying on an up-to-date database is a
// no-op.
func (s *sqliteStore) migrate(ctx context.Context) error {
	for i, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
	}
	return nil
}

// timestampLayout is the on-disk timestamp format. RFC3339Nano gives
// human-readable ISO-8601 with sub-second precision and round-trips
// cleanly through time.Parse. Matches credstore.
const timestampLayout = time.RFC3339Nano

// Create inserts a new connection row. The ConnectionID is
// generated here (callers leave it empty on the input); CreatedAt and
// UpdatedAt are stamped to now and written back to the passed-in
// struct so the caller can return them in the API response.
//
// Returns ErrConnectionConflict when a row already exists for
// (Provider, RepoFullName). All other failures wrap the underlying
// database error.
func (s *sqliteStore) Create(ctx context.Context, conn *IaCConnection) error {
	if conn == nil {
		return errors.New("iacconnstore: Create: conn is required")
	}
	if conn.Provider == "" {
		return errors.New("iacconnstore: Create: Provider is required")
	}
	if conn.AuthKind == "" {
		return errors.New("iacconnstore: Create: AuthKind is required")
	}
	if conn.RepoFullName == "" {
		return errors.New("iacconnstore: Create: RepoFullName is required")
	}
	if conn.DefaultBranch == "" {
		return errors.New("iacconnstore: Create: DefaultBranch is required")
	}
	if conn.RepoLayout == "" {
		return errors.New("iacconnstore: Create: RepoLayout is required")
	}
	if len(conn.CredCiphertext) == 0 {
		return errors.New("iacconnstore: Create: CredCiphertext is required (callers must seal via MarshalGitHubPATCreds)")
	}

	// PlacementMap is serialized as a JSON array TEXT column. A nil
	// slice becomes "[]" so the column is NOT NULL and readers don't
	// need to special-case empty.
	placement := conn.PlacementMap
	if placement == nil {
		placement = []PlacementMapEntry{}
	}
	placementJSON, err := json.Marshal(placement)
	if err != nil {
		return fmt.Errorf("iacconnstore: marshal PlacementMap: %w", err)
	}

	now := s.timeNow()
	conn.ConnectionID = s.newUUID()
	conn.CreatedAt = now
	conn.UpdatedAt = now

	const stmt = `
		INSERT INTO iac_connections (
			connection_id, provider, auth_kind, repo_full_name, default_branch,
			repo_layout, branch_prefix, reviewer_team_handle,
			placement_map_json, cred_ciphertext,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if _, err := s.db.ExecContext(ctx, stmt,
		conn.ConnectionID,
		conn.Provider,
		conn.AuthKind,
		conn.RepoFullName,
		conn.DefaultBranch,
		conn.RepoLayout,
		nullableString(conn.BranchPrefix),
		nullableString(conn.ReviewerTeamHandle),
		string(placementJSON),
		conn.CredCiphertext,
		now.Format(timestampLayout),
		now.Format(timestampLayout),
	); err != nil {
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("%w: provider=%s repo=%s", ErrConnectionConflict, conn.Provider, conn.RepoFullName)
		}
		return fmt.Errorf("iacconnstore: insert iac_connection: %w", err)
	}
	return nil
}

// Get returns the connection row for the given ConnectionID. Returns
// ErrConnectionNotFound when no row matches.
func (s *sqliteStore) Get(ctx context.Context, connectionID string) (*IaCConnection, error) {
	if connectionID == "" {
		return nil, errors.New("iacconnstore: Get: connectionID is required")
	}
	const stmt = `
		SELECT connection_id, provider, auth_kind, repo_full_name, default_branch,
		       repo_layout, branch_prefix, reviewer_team_handle,
		       placement_map_json, cred_ciphertext,
		       created_at, updated_at
		FROM iac_connections
		WHERE connection_id = ?
	`
	row := s.db.QueryRowContext(ctx, stmt, connectionID)
	conn, err := scanConnection(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConnectionNotFound
		}
		return nil, err
	}
	return conn, nil
}

// GetByRepoFullName returns the most recently created connection row
// whose repo_full_name matches the supplied value, or
// ErrConnectionNotFound when no row exists. The substrate's unique
// index on (provider, repo_full_name) caps the result at one row per
// provider; the ORDER BY + LIMIT 1 is the forward-compat hedge for the
// slice-2 question of allowing multiple connections per repo.
func (s *sqliteStore) GetByRepoFullName(ctx context.Context, repoFullName string) (*IaCConnection, error) {
	if repoFullName == "" {
		return nil, errors.New("iacconnstore: GetByRepoFullName: repoFullName is required")
	}
	const stmt = `
		SELECT connection_id, provider, auth_kind, repo_full_name, default_branch,
		       repo_layout, branch_prefix, reviewer_team_handle,
		       placement_map_json, cred_ciphertext,
		       created_at, updated_at
		FROM iac_connections
		WHERE repo_full_name = ?
		ORDER BY created_at DESC
		LIMIT 1
	`
	row := s.db.QueryRowContext(ctx, stmt, repoFullName)
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
func (s *sqliteStore) List(ctx context.Context) ([]*IaCConnection, error) {
	const stmt = `
		SELECT connection_id, provider, auth_kind, repo_full_name, default_branch,
		       repo_layout, branch_prefix, reviewer_team_handle,
		       placement_map_json, cred_ciphertext,
		       created_at, updated_at
		FROM iac_connections
		ORDER BY created_at ASC, connection_id ASC
	`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("iacconnstore: list iac_connections: %w", err)
	}
	defer rows.Close()

	var out []*IaCConnection
	for rows.Next() {
		conn, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iacconnstore: list iac_connections rows: %w", err)
	}
	return out, nil
}

// Delete removes the row for the given ConnectionID. Idempotent.
func (s *sqliteStore) Delete(ctx context.Context, connectionID string) error {
	if connectionID == "" {
		return errors.New("iacconnstore: Delete: connectionID is required")
	}
	const stmt = `DELETE FROM iac_connections WHERE connection_id = ?`
	if _, err := s.db.ExecContext(ctx, stmt, connectionID); err != nil {
		return fmt.Errorf("iacconnstore: delete iac_connection %s: %w", connectionID, err)
	}
	return nil
}

// UpdatePlacementMap replaces the PlacementMap on the row and stamps
// UpdatedAt. No other column is touched.
func (s *sqliteStore) UpdatePlacementMap(ctx context.Context, connectionID string, entries []PlacementMapEntry) error {
	if connectionID == "" {
		return errors.New("iacconnstore: UpdatePlacementMap: connectionID is required")
	}
	if entries == nil {
		entries = []PlacementMapEntry{}
	}
	placementJSON, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("iacconnstore: marshal PlacementMap: %w", err)
	}
	now := s.timeNow()

	const stmt = `
		UPDATE iac_connections
		SET placement_map_json = ?, updated_at = ?
		WHERE connection_id = ?
	`
	res, err := s.db.ExecContext(ctx, stmt,
		string(placementJSON),
		now.Format(timestampLayout),
		connectionID,
	)
	if err != nil {
		return fmt.Errorf("iacconnstore: update placement_map for %s: %w", connectionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("iacconnstore: rows affected for %s: %w", connectionID, err)
	}
	if affected == 0 {
		return ErrConnectionNotFound
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
func scanConnection(r rowScanner) (*IaCConnection, error) {
	var (
		conn               IaCConnection
		branchPrefix       sql.NullString
		reviewerTeamHandle sql.NullString
		placementJSON      string
		createdAt          string
		updatedAt          string
	)
	if err := r.Scan(
		&conn.ConnectionID,
		&conn.Provider,
		&conn.AuthKind,
		&conn.RepoFullName,
		&conn.DefaultBranch,
		&conn.RepoLayout,
		&branchPrefix,
		&reviewerTeamHandle,
		&placementJSON,
		&conn.CredCiphertext,
		&createdAt,
		&updatedAt,
	); err != nil {
		return nil, err
	}
	if branchPrefix.Valid {
		conn.BranchPrefix = branchPrefix.String
	}
	if reviewerTeamHandle.Valid {
		conn.ReviewerTeamHandle = reviewerTeamHandle.String
	}
	if err := json.Unmarshal([]byte(placementJSON), &conn.PlacementMap); err != nil {
		return nil, fmt.Errorf("iacconnstore: unmarshal placement_map for %s: %w", conn.ConnectionID, err)
	}
	if conn.PlacementMap == nil {
		conn.PlacementMap = []PlacementMapEntry{}
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
// optional columns (branch_prefix, reviewer_team_handle) don't
// store "" — that way operators querying the DB directly can
// distinguish "the operator left this blank" from "the operator
// typed an empty string".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueConstraintErr reports whether err is the SQLite-driver
// surface of a UNIQUE constraint failure. The substrate maps this
// signal onto ErrConnectionConflict at the Create boundary so
// handlers see a typed error rather than a driver-specific string.
//
// mattn/go-sqlite3 wraps the SQLite error code in an error whose
// message contains "UNIQUE constraint failed". Pattern-matching the
// string is the documented escape hatch — go-sqlite3 also exposes
// sqlite3.Error with an extended code, but reaching into the driver
// type would couple this package to the driver in a way that
// complicates swapping it later.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
