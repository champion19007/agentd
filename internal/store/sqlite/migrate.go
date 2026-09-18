package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/champion19007/agentd/migrations"
)

// Migration strategy.
//
// Forward-only, numbered, each in its own transaction, recorded in a table
// that is itself created idempotently. There is no down migration, on purpose:
// a rollback that runs against production data is a second untested code path
// executed at the worst possible moment. Going back means restoring a backup,
// which is a thing an operator can actually reason about -- and which agentd
// backup exists to make cheap.
//
// A migration that has already been applied is skipped. A file whose version
// is lower than the highest applied version but which has never been applied is
// an error rather than a surprise: it means two branches numbered migrations
// independently, and applying it now would run it against a schema it was
// never written for.

// migration is one numbered step.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate brings the database up to date. It runs on the writer pool, which
// has a single connection, so two callers cannot migrate at once.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("sqlite: creating migration table: %w", err)
	}

	applied, highest, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	all, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if m.version < highest {
			return fmt.Errorf(
				"sqlite: migration %04d (%s) has never been applied but the database is already at %04d; "+
					"two branches numbered migrations independently and this one cannot be applied safely",
				m.version, m.name, highest)
		}
		if err := applyOne(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// applyOne runs one migration and records it, both or neither.
func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: starting migration %04d: %w", m.version, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("sqlite: applying migration %04d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, datetime('now'))`,
		m.version, m.name,
	); err != nil {
		return fmt.Errorf("sqlite: recording migration %04d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: committing migration %04d: %w", m.version, err)
	}
	return nil
}

// appliedVersions returns which migrations have run and the highest of them.
func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, int, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: reading applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	highest := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, 0, err
		}
		applied[v] = true
		if v > highest {
			highest = v
		}
	}
	return applied, highest, rows.Err()
}

// loadMigrations reads the embedded files, in version order.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading embedded migrations: %w", err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("sqlite: reading migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: name, sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })

	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("sqlite: two migrations share version %04d", out[i].version)
		}
	}
	return out, nil
}

// parseName splits "0001_init.sql" into 1 and "init".
func parseName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	num, name, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("sqlite: migration %q is not named <version>_<description>.sql", filename)
	}
	version, err := strconv.Atoi(num)
	if err != nil {
		return 0, "", fmt.Errorf("sqlite: migration %q has a non-numeric version: %w", filename, err)
	}
	if version < 1 {
		return 0, "", fmt.Errorf("sqlite: migration %q has version %d; versions start at 1", filename, version)
	}
	return version, name, nil
}

// SchemaVersion returns the highest applied migration, for diagnostics.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.reader.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}
