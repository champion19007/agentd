package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Maintenance is the work an operator asks for, not work that happens to them.
//
// Retention deletes data, and a tool whose selling point is that it never acts
// without permission should not quietly delete things on a timer at three in
// the morning. So there is no scheduled job here and no database trigger: GC
// runs when something calls GC, it reports what it removed, and it stops when
// its context is cancelled.

// GC applies a retention policy and reports what it removed.
//
// The policy comes from the domain. The store executes it and never decides
// what may be forgotten -- in particular it does not reimplement "keep the
// newest N", because the domain's version of that rule knowingly exceeds the
// count rather than drop the last known-good capture, and a store reinventing
// it from the description would get that exactly wrong.
func (s *Store) GC(ctx context.Context, policy domain.Retention, now time.Time) (domain.Sweep, error) {
	if err := policy.Validate(); err != nil {
		return domain.Sweep{}, err
	}

	var sweep domain.Sweep

	checkIDs, err := s.allCheckIDs(ctx)
	if err != nil {
		return sweep, err
	}

	// Captures first, per check, because the decision needs the whole index in
	// hand: which capture is protected depends on all the others.
	for _, id := range checkIDs {
		if err := ctx.Err(); err != nil {
			return sweep, err
		}
		n, err := s.gcSnapshots(ctx, id, policy)
		if err != nil {
			return sweep, err
		}
		sweep.Snapshots += n
	}

	runs, err := s.gcRuns(ctx, policy, now)
	if err != nil {
		return sweep, err
	}
	sweep.Runs = runs

	incidents, err := s.gcIncidents(ctx, policy, now)
	if err != nil {
		return sweep, err
	}
	sweep.Incidents = incidents

	// Passive WAL checkpoint: folds completed log frames back to the main database file
	// without blocking active readers or forcing locks.
	var busy, logFrames, checkpointed int
	_ = s.writer.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed)

	return sweep, nil
}

func (s *Store) allCheckIDs(ctx context.Context) ([]domain.CheckID, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT id FROM checks WHERE tenant_id = ? ORDER BY id`, s.tenant)
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing checks: %w", err)
	}
	defer rows.Close()

	var out []domain.CheckID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, domain.CheckID(id))
	}
	return out, rows.Err()
}

// gcSnapshots prunes one check's captures.
func (s *Store) gcSnapshots(ctx context.Context, id domain.CheckID, policy domain.Retention) (int, error) {
	index, err := s.Snapshots(ctx, id)
	if err != nil {
		return 0, err
	}

	expired := policy.ExpiredSnapshots(index)
	if len(expired) == 0 {
		return 0, nil
	}

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.DeleteSnapshots(ctx, id, expired)
	}); err != nil {
		return 0, err
	}
	return len(expired), nil
}

// gcRuns deletes finished runs past the retention window.
//
// Unfinished runs are never deleted by age. A pending run that is ninety days
// old is a bug worth finding, and sweeping it away would hide it.
func (s *Store) gcRuns(ctx context.Context, policy domain.Retention, now time.Time) (int, error) {
	cutoff := mustEncodeTime(policy.RunCutoff(now))

	var removed int
	err := s.WithTx(ctx, func(ctx context.Context, t ports.Tx) error {
		res, err := t.(tx).db.ExecContext(ctx, `
			DELETE FROM runs
			 WHERE tenant_id = ?
			   AND state NOT IN ('pending', 'running')
			   AND ended_at IS NOT NULL
			   AND ended_at < ?`, s.tenant, cutoff)
		if err != nil {
			return fmt.Errorf("sqlite: sweeping runs: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		removed = int(n)
		return nil
	})
	return removed, err
}

// gcIncidents deletes incidents closed longer ago than the window allows.
//
// An open incident is never deleted, however old. Deleting one would silently
// withdraw a question Agentd has put to a human, and a withdrawn question looks
// exactly like an answered one from the outside.
func (s *Store) gcIncidents(ctx context.Context, policy domain.Retention, now time.Time) (int, error) {
	cutoff := mustEncodeTime(policy.IncidentCutoff(now))

	var removed int
	err := s.WithTx(ctx, func(ctx context.Context, t ports.Tx) error {
		res, err := t.(tx).db.ExecContext(ctx, `
			DELETE FROM incidents
			 WHERE tenant_id = ?
			   AND state NOT IN ('open', 'awaiting_approval')
			   AND closed_at IS NOT NULL
			   AND closed_at < ?`, s.tenant, cutoff)
		if err != nil {
			return fmt.Errorf("sqlite: sweeping incidents: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		removed = int(n)
		return nil
	})
	return removed, err
}

// --- backup -----------------------------------------------------------------

// ErrBackupExists reports a backup destination that is already occupied.
var ErrBackupExists = errors.New("sqlite: the backup destination already exists")

// Backup writes a consistent copy of the database to path, safely, while the
// database is in use.
//
// It uses VACUUM INTO rather than copying files. That distinction is the whole
// point of this function: in WAL mode the file on disk is not the database.
// Recent commits live in the -wal sidecar, so copying the main file with cp,
// rsync or filepath.Copy while anything is writing produces a backup that is
// either missing the newest transactions or torn across a checkpoint. It
// usually opens fine, which is what makes it dangerous -- the damage is found
// during a restore, at the worst moment.
//
// VACUUM INTO asks SQLite to build a fresh, fully checkpointed database from a
// consistent read of the current one. It takes a read transaction, so writers
// are not blocked out for its duration, and the result is a single file with no
// sidecars that can be copied around freely.
func (s *Store) Backup(ctx context.Context, path string) (int64, error) {
	if path == "" {
		return 0, errors.New("sqlite: a backup path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("sqlite: resolving backup path %q: %w", path, err)
	}

	// VACUUM INTO refuses an existing file, but failing here gives a better
	// message than SQLite's, and does it before any work is done.
	if _, err := os.Stat(abs); err == nil {
		return 0, fmt.Errorf("%w: %s", ErrBackupExists, abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("sqlite: checking backup path %q: %w", abs, err)
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, fmt.Errorf("sqlite: preparing backup directory: %w", err)
	}

	// Checkpoint first so the WAL is folded into the main database. This is
	// not required for correctness -- VACUUM INTO reads a consistent view
	// either way -- but it keeps the WAL from growing without bound on a
	// database that is only ever read from by long-lived readers.
	if err := s.Checkpoint(ctx); err != nil {
		return 0, err
	}

	// Run on the writer pool. VACUUM INTO only takes a read lock, but the
	// writer pool is where statements that are not plain reads belong, and the
	// reader pool is query_only.
	if _, err := s.writer.ExecContext(ctx, `VACUUM INTO ?`, abs); err != nil {
		return 0, fmt.Errorf("sqlite: writing backup to %q: %w", abs, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return 0, fmt.Errorf("sqlite: backup was written but cannot be read back: %w", err)
	}
	return info.Size(), nil
}

// Checkpoint folds the write-ahead log back into the main database file.
//
// TRUNCATE mode is used rather than PASSIVE so that the WAL is actually reset
// rather than merely caught up. It waits for readers to finish rather than
// forcing them out, so a checkpoint under load may do less than asked; that is
// the correct trade for a maintenance operation and is why Backup does not
// depend on its result.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	err := s.writer.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		return fmt.Errorf("sqlite: checkpointing: %w", err)
	}
	return nil
}

// VerifyBackup opens a backup read-only and confirms it is a usable database
// at the expected schema version.
//
// A backup nobody has opened is a hope, not a backup. This is cheap enough to
// run every time one is taken.
func VerifyBackup(ctx context.Context, path string) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("sqlite: resolving backup path %q: %w", path, err)
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)", filepath.ToSlash(abs))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, fmt.Errorf("sqlite: backup at %q could not be opened: %w", path, err)
	}
	defer db.Close()

	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return 0, fmt.Errorf("sqlite: checking backup at %q: %w", path, err)
	}
	if result != "ok" {
		return 0, fmt.Errorf("sqlite: backup at %q failed its integrity check: %s", path, result)
	}

	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("sqlite: reading backup schema version at %q: %w", path, err)
	}
	return int(version.Int64), nil
}
