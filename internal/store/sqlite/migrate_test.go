package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/champion19007/agentd/internal/core/domain"
)

// TestUpgradeFromInitialSchema is the real test of the migration runner: a
// database created at 0001, with data in it, must come forward to 0002 without
// losing anything.
//
// Idempotency tests only prove a fresh database is not migrated twice. This one
// proves the upgrade path an operator will actually take works, which is the
// path that has no undo.
func TestUpgradeFromInitialSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(all) < 2 {
		t.Skip("only one migration exists; there is no upgrade to test yet")
	}

	// Build a database at version 1 only.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("creating db file: %v", err)
	}
	f.Close()

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatalf("creating migration table: %v", err)
	}
	if err := applyOne(ctx, raw, all[0]); err != nil {
		t.Fatalf("applying 0001: %v", err)
	}

	// Data written under the old schema, including a capture in the shape
	// 0001 used.
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO checks (tenant_id, id, enabled, active_version, created_at, updated_at)
		VALUES ('default', 'chk-1', 1, 1, '2026-03-01T12:00:00Z', '2026-03-01T12:00:00Z')`); err != nil {
		t.Fatalf("seeding a check: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO snapshots (tenant_id, check_id, id, content_type, size_bytes,
			fingerprint, captured_at, known_good, compression, body)
		VALUES ('default', 'chk-1', 'sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824', 'text/html', 5, 'fp-v1',
			'2026-03-01T12:00:00Z', 1, 'none', X'68656C6C6F')`); err != nil {
		t.Fatalf("seeding a capture: %v", err)
	}
	raw.Close()
	_ = os.Chmod(path, 0600)

	// Opening applies everything outstanding.
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open on an old database: %v", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != all[len(all)-1].version {
		t.Errorf("schema version = %d, want %d", version, all[len(all)-1].version)
	}

	// The capture came across, body and known-good flag intact. Losing the
	// known-good flag here would silently disable repair for every existing
	// check, which is the kind of damage nobody notices until it matters.
	index, err := store.Snapshots(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if index.Len() != 1 {
		t.Fatalf("%d captures survived the upgrade, want 1", index.Len())
	}
	good, err := index.LatestKnownGood()
	if err != nil {
		t.Fatalf("the known-good flag did not survive the upgrade: %v", err)
	}
	if string(good.Body()) != "hello" {
		t.Errorf("Body = %q, want the bytes stored under the old schema", good.Body())
	}
	// The address has to be the real digest of the bytes. Reading a capture
	// verifies that, so a row whose body was tampered with -- or corrupted on
	// disk -- is refused rather than used to verify a repair.
	if got := domain.Digest([]byte("hello")); good.ID() != got {
		t.Errorf("ID = %q, want %q", good.ID(), got)
	}
}

func TestMajorMigrationClassificationAndPending(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pending.db")

	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Initially, all migrations are applied
	pending, hasMajor, err := store.PendingMigrations(ctx)
	if err != nil {
		t.Fatalf("PendingMigrations: %v", err)
	}
	if len(pending) != 0 || hasMajor {
		t.Errorf("expected 0 pending migrations, got %d (hasMajor=%v)", len(pending), hasMajor)
	}

	// Verify isMajor classification
	if !isMajor("0003_major_incident_graph.sql") {
		t.Error("expected 'major' in name to be classified as major")
	}
	if !isMajor("0004_breaking_policy.sql") {
		t.Error("expected 'breaking' in name to be classified as major")
	}
	if isMajor("0001_init.sql") {
		t.Error("expected 'init' not to be classified as major")
	}
}
