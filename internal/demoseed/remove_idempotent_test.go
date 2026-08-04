package demoseed_test

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/demoseed"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// TestSeedThenRemove_NoError reproduces the "cannot remove demo data" bug:
// enable demo data then remove it, and Remove must not error.
func TestSeedThenRemove_NoError(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.NewSQLiteStorage(filepath.Join(dir, "demo.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()

	if _, err := demoseed.Seed(ctx, store, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := demoseed.Remove(ctx, store); err != nil {
		t.Fatalf("REMOVE FAILED: %v", err)
	}
	// Second remove must also be clean (idempotent teardown).
	if err := demoseed.Remove(ctx, store); err != nil {
		t.Fatalf("SECOND REMOVE FAILED (not idempotent): %v", err)
	}
}
